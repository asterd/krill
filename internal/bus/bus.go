package bus

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krill/krill/internal/telemetry"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

type Envelope struct {
	ID             string            `json:"id"`
	ClientID       string            `json:"client_id"`
	ThreadID       string            `json:"thread_id"`
	Role           Role              `json:"role"`
	Text           string            `json:"text"`
	Attachments    []Attachment      `json:"attachments,omitempty"`
	ToolCalls      []ToolCall        `json:"tool_calls,omitempty"`
	ToolCallID     string            `json:"tool_call_id,omitempty"`
	SourceProtocol string            `json:"source_protocol"`
	Meta           map[string]string `json:"meta,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
}

type Attachment struct {
	Type string `json:"type"`
	URL  string `json:"url,omitempty"`
	Data []byte `json:"data,omitempty"`
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

const InboundKey = "__inbound__"

var (
	replyPrefixMu sync.RWMutex
	replyPrefix   = "__reply__"

	// ErrBackpressure indicates that at least one subscriber queue is full.
	ErrBackpressure = errors.New("bus backpressure: subscriber queue full")
)

func SetReplyPrefix(prefix string) {
	replyPrefixMu.Lock()
	defer replyPrefixMu.Unlock()
	clean := strings.TrimSpace(prefix)
	if clean == "" {
		clean = "__reply__"
	}
	replyPrefix = clean
}

func ReplyKey(protocol string) string {
	replyPrefixMu.RLock()
	prefix := replyPrefix
	replyPrefixMu.RUnlock()
	return prefix + ":" + protocol
}

// Bus is the compatibility facade used by the current runtime.
type Bus interface {
	Publish(ctx context.Context, key string, env *Envelope) error
	Subscribe(key string) <-chan *Envelope
	Unsubscribe(key string)
}

// Subscription is the explicit backbone subscription contract used internally.
type Subscription interface {
	Key() string
	C() <-chan *Envelope
	Ack(*Envelope)
	Close()
}

// Backbone is the richer event delivery contract introduced by M4.5.
type Backbone interface {
	Bus
	SubscribeQueue(key string) Subscription
	SubscriberCount(key string) int
}

type localBackbone struct {
	bufSize int

	mu      sync.RWMutex
	topics  map[string]map[uint64]*subscriber
	nextSub atomic.Uint64
}

type subscriber struct {
	id     uint64
	key    string
	ch     chan *Envelope
	parent *localBackbone
}

func NewLocal(bufSize int) Bus {
	if bufSize <= 0 {
		bufSize = 1
	}
	return &localBackbone{
		bufSize: bufSize,
		topics:  make(map[string]map[uint64]*subscriber),
	}
}

func (b *localBackbone) Publish(ctx context.Context, key string, env *Envelope) error {
	if env == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.RLock()
	subs := make([]*subscriber, 0, len(b.topics[key]))
	for _, sub := range b.topics[key] {
		subs = append(subs, sub)
	}
	for _, sub := range subs {
		if len(sub.ch) >= cap(sub.ch) {
			b.mu.RUnlock()
			return ErrBackpressure
		}
	}
	for _, sub := range subs {
		select {
		case sub.ch <- env:
		case <-ctx.Done():
			b.mu.RUnlock()
			return ctx.Err()
		default:
			b.mu.RUnlock()
			return ErrBackpressure
		}
	}
	b.mu.RUnlock()
	if key == InboundKey {
		var depth int
		for _, sub := range subs {
			depth += len(sub.ch)
		}
		telemetry.SetGauge(telemetry.MetricInboundQueueDepth, int64(depth), nil)
	}
	return nil
}

func (b *localBackbone) Subscribe(key string) <-chan *Envelope {
	return b.SubscribeQueue(key).C()
}

func (b *localBackbone) SubscribeQueue(key string) Subscription {
	sub := &subscriber{
		id:     b.nextSub.Add(1),
		key:    key,
		ch:     make(chan *Envelope, b.bufSize),
		parent: b,
	}
	b.mu.Lock()
	if b.topics[key] == nil {
		b.topics[key] = make(map[uint64]*subscriber)
	}
	b.topics[key][sub.id] = sub
	b.mu.Unlock()
	return sub
}

func (b *localBackbone) SubscriberCount(key string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.topics[key])
}

func (b *localBackbone) Unsubscribe(key string) {
	b.mu.Lock()
	subs := b.topics[key]
	delete(b.topics, key)
	b.mu.Unlock()
	for _, sub := range subs {
		close(sub.ch)
	}
}

func (s *subscriber) Key() string         { return s.key }
func (s *subscriber) C() <-chan *Envelope { return s.ch }
func (s *subscriber) Ack(_ *Envelope)     {}
func (s *subscriber) Close()              { s.parent.removeSubscriber(s.key, s.id, s.ch) }

func (b *localBackbone) removeSubscriber(key string, id uint64, ch chan *Envelope) {
	b.mu.Lock()
	if subs, ok := b.topics[key]; ok {
		delete(subs, id)
		if len(subs) == 0 {
			delete(b.topics, key)
		}
	}
	b.mu.Unlock()
	close(ch)
}

// Package bus — the universal internal message backbone.
//
// Design contract:
//   - Protocols publish INBOUND  messages to key "__inbound__"
//   - Agent loops publish OUTBOUND messages to key "__reply__:<protocol>"
//   - Protocols subscribe to "__reply__:<their_name>" and filter by ClientID in Meta
//
// This routing model means:
//   - The bus is purely a pub/sub channel map; no routing logic lives here.
//   - Swapping to NATS/Redis: implement Bus interface, wire in main.go.
package bus

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/krill/krill/internal/telemetry"
)

// ─── Envelope ─────────────────────────────────────────────────────────────────

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// Envelope is the single internal message format all protocols normalize to.
type Envelope struct {
	ID string `json:"id"`
	// ClientID identifies the end-user session (e.g. "tg:12345678", "http:uuid")
	ClientID string `json:"client_id"`
	// ThreadID is the conversation thread (often == ClientID, but clients may have multiple threads)
	ThreadID       string            `json:"thread_id"`
	Role           Role              `json:"role"`
	Text           string            `json:"text"` // primary text content
	Attachments    []Attachment      `json:"attachments,omitempty"`
	ToolCalls      []ToolCall        `json:"tool_calls,omitempty"`
	ToolCallID     string            `json:"tool_call_id,omitempty"` // for role=tool replies
	SourceProtocol string            `json:"source_protocol"`        // originating plugin name
	Meta           map[string]string `json:"meta,omitempty"`         // protocol-specific metadata
	CreatedAt      time.Time         `json:"created_at"`
}

type Attachment struct {
	Type string `json:"type"` // "image_url" | "file"
	URL  string `json:"url,omitempty"`
	Data []byte `json:"data,omitempty"`
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded
}

// InboundKey is the bus channel all protocols publish user messages to.
const InboundKey = "__inbound__"

var (
	replyPrefixMu sync.RWMutex
	replyPrefix   = "__reply__"
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

// ReplyKey builds the per-protocol reply channel key.
func ReplyKey(protocol string) string {
	replyPrefixMu.RLock()
	prefix := replyPrefix
	replyPrefixMu.RUnlock()
	return prefix + ":" + protocol
}

// ─── Bus interface ────────────────────────────────────────────────────────────

// Bus is the pub/sub backbone. One channel per key, bounded.
type Bus interface {
	// Publish sends an envelope to the channel identified by key.
	Publish(ctx context.Context, key string, env *Envelope) error
	// Subscribe returns the receive channel for a key (created on first call).
	Subscribe(key string) <-chan *Envelope
	// Unsubscribe removes and closes the channel.
	Unsubscribe(key string)
}

// ─── Local in-process implementation ─────────────────────────────────────────

type localBus struct {
	bufSize int
	mu      sync.RWMutex
	chans   map[string]chan *Envelope
}

func NewLocal(bufSize int) Bus {
	return &localBus{bufSize: bufSize, chans: make(map[string]chan *Envelope)}
}

func (b *localBus) Publish(ctx context.Context, key string, env *Envelope) error {
	traceID := ""
	parentSpan := ""
	if env != nil && env.Meta != nil {
		traceID = env.Meta["trace_id"]
		parentSpan = env.Meta["ingress_span"]
	}
	span := telemetry.StartSpan(nil, traceID, parentSpan, "bus.publish", "key", key)
	defer span.End(nil, "key", key)

	ch := b.getOrCreate(key)
	select {
	case ch <- env:
	case <-ctx.Done():
		return ctx.Err()
	default:
		// Ring-buffer: evict oldest to make room
		select {
		case <-ch:
		default:
		}
		ch <- env
	}
	if key == InboundKey {
		telemetry.SetGauge(telemetry.MetricInboundQueueDepth, int64(len(ch)), nil)
	}
	return nil
}

func (b *localBus) Subscribe(key string) <-chan *Envelope {
	return b.getOrCreate(key)
}

func (b *localBus) Unsubscribe(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.chans[key]; ok {
		close(ch)
		delete(b.chans, key)
	}
}

func (b *localBus) getOrCreate(key string) chan *Envelope {
	b.mu.RLock()
	ch, ok := b.chans[key]
	b.mu.RUnlock()
	if ok {
		return ch
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok = b.chans[key]; ok {
		return ch
	}
	ch = make(chan *Envelope, b.bufSize)
	b.chans[key] = ch
	return ch
}

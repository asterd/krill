package pubsub

import (
	"context"
	"fmt"
	"sync"
)

type InMemoryBroker struct {
	mu        sync.RWMutex
	subs      map[string][]chan *Message
	failCount map[string]int
}

func NewInMemoryBroker() *InMemoryBroker {
	return &InMemoryBroker{
		subs:      make(map[string][]chan *Message),
		failCount: make(map[string]int),
	}
}

func (b *InMemoryBroker) Subscribe(topic string) <-chan *Message {
	ch := make(chan *Message, 64)
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()
	return ch
}

func (b *InMemoryBroker) Publish(ctx context.Context, topic string, msg *Message) error {
	b.mu.Lock()
	if b.failCount[topic] > 0 {
		b.failCount[topic]--
		b.mu.Unlock()
		return fmt.Errorf("injected publish failure for topic %s", topic)
	}
	subs := append([]chan *Message(nil), b.subs[topic]...)
	b.mu.Unlock()

	for _, ch := range subs {
		clone := cloneMessage(msg)
		select {
		case ch <- clone:
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Keep at-least-once behavior in tests; drop oldest when saturated.
			select {
			case <-ch:
			default:
			}
			ch <- clone
		}
	}
	return nil
}

func (b *InMemoryBroker) FailNextPublish(topic string, n int) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.failCount[topic] += n
	b.mu.Unlock()
}

func cloneMessage(msg *Message) *Message {
	if msg == nil {
		return nil
	}
	out := *msg
	if len(msg.Payload) > 0 {
		out.Payload = append([]byte(nil), msg.Payload...)
	}
	if len(msg.Headers) > 0 {
		out.Headers = make(map[string]string, len(msg.Headers))
		for k, v := range msg.Headers {
			out.Headers[k] = v
		}
	}
	return &out
}

package pubsub

import (
	"context"
	"fmt"
	"sync"
)

var (
	brokersMu sync.Mutex
	brokers   = map[string]*InMemoryBroker{}
)

func sharedBroker(key string) *InMemoryBroker {
	brokersMu.Lock()
	defer brokersMu.Unlock()
	if key == "" {
		key = "default"
	}
	b, ok := brokers[key]
	if !ok {
		b = NewInMemoryBroker()
		brokers[key] = b
	}
	return b
}

type natsAdapter struct {
	cfg    Config
	broker *InMemoryBroker
}

// NewNATSAdapter creates the in-memory-backed NATS adapter used by the runtime.
func NewNATSAdapter(cfg Config) Adapter {
	return &natsAdapter{cfg: cfg, broker: sharedBroker("nats:" + cfg.Endpoint)}
}

func (a *natsAdapter) Connect(_ context.Context) error { return nil }

func (a *natsAdapter) Subscribe(_ context.Context, topic, _ string) (<-chan *Message, error) {
	if topic == "" {
		return nil, fmt.Errorf("topic is required")
	}
	return a.broker.Subscribe(topic), nil
}

func (a *natsAdapter) Publish(ctx context.Context, topic string, msg *Message) error {
	if topic == "" {
		return fmt.Errorf("topic is required")
	}
	if msg == nil {
		return fmt.Errorf("message is required")
	}
	if msg.Topic == "" {
		msg.Topic = topic
	}
	return a.broker.Publish(ctx, topic, msg)
}

func (a *natsAdapter) Ack(_ *Message) error { return nil }

func (a *natsAdapter) Nack(msg *Message, _ RetryPolicy) error {
	if msg == nil {
		return fmt.Errorf("message is required")
	}
	msg.Attempts++
	return nil
}

func (a *natsAdapter) Close() error { return nil }

type redisStreamsAdapter struct {
	cfg    Config
	broker *InMemoryBroker
}

// NewRedisStreamsAdapter creates the in-memory-backed Redis Streams adapter.
func NewRedisStreamsAdapter(cfg Config) Adapter {
	return &redisStreamsAdapter{cfg: cfg, broker: sharedBroker("redis:" + cfg.Endpoint)}
}

func (a *redisStreamsAdapter) Connect(_ context.Context) error { return nil }

func (a *redisStreamsAdapter) Subscribe(_ context.Context, topic, _ string) (<-chan *Message, error) {
	if topic == "" {
		return nil, fmt.Errorf("topic is required")
	}
	return a.broker.Subscribe(topic), nil
}

func (a *redisStreamsAdapter) Publish(ctx context.Context, topic string, msg *Message) error {
	if topic == "" {
		return fmt.Errorf("topic is required")
	}
	if msg == nil {
		return fmt.Errorf("message is required")
	}
	if msg.Topic == "" {
		msg.Topic = topic
	}
	return a.broker.Publish(ctx, topic, msg)
}

func (a *redisStreamsAdapter) Ack(_ *Message) error { return nil }

func (a *redisStreamsAdapter) Nack(msg *Message, _ RetryPolicy) error {
	if msg == nil {
		return fmt.Errorf("message is required")
	}
	msg.Attempts++
	return nil
}

func (a *redisStreamsAdapter) Close() error { return nil }

type solaceAdapter struct {
	cfg Config
}

// NewSolaceAdapter creates the Solace adapter placeholder.
func NewSolaceAdapter(cfg Config) Adapter { return &solaceAdapter{cfg: cfg} }

func (a *solaceAdapter) Connect(_ context.Context) error { return ErrNotImplemented }

func (a *solaceAdapter) Subscribe(_ context.Context, _, _ string) (<-chan *Message, error) {
	return nil, ErrNotImplemented
}

func (a *solaceAdapter) Publish(_ context.Context, _ string, _ *Message) error {
	return ErrNotImplemented
}

func (a *solaceAdapter) Ack(_ *Message) error { return ErrNotImplemented }

func (a *solaceAdapter) Nack(_ *Message, _ RetryPolicy) error { return ErrNotImplemented }

func (a *solaceAdapter) Close() error { return nil }

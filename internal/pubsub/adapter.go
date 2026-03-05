package pubsub

import (
	"context"
	"errors"
	"time"
)

var ErrNotImplemented = errors.New("pubsub adapter not implemented")

// RetryPolicy defines retry and DLQ behavior for PubSub messages.
type RetryPolicy struct {
	MaxRetries int
	Backoff    time.Duration
	MaxBackoff time.Duration
	DLQTopic   string
}

// Message is the transport-level unit exchanged with PubSub adapters.
type Message struct {
	ID       string
	Topic    string
	Payload  []byte
	Headers  map[string]string
	Attempts int
}

// Adapter is the common interface implemented by PubSub backends.
type Adapter interface {
	Connect(ctx context.Context) error
	Subscribe(ctx context.Context, topic, group string) (<-chan *Message, error)
	Publish(ctx context.Context, topic string, msg *Message) error
	Ack(msg *Message) error
	Nack(msg *Message, retry RetryPolicy) error
	Close() error
}

// Config describes how to connect to a PubSub backend.
type Config struct {
	BrokerType string
	Endpoint   string
}

// NewAdapter selects a PubSub adapter implementation from config.
func NewAdapter(cfg Config) Adapter {
	switch cfg.BrokerType {
	case "nats":
		return NewNATSAdapter(cfg)
	case "redis_streams":
		return NewRedisStreamsAdapter(cfg)
	case "solace":
		return NewSolaceAdapter(cfg)
	default:
		return NewNATSAdapter(cfg)
	}
}

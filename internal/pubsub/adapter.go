package pubsub

import (
	"context"
	"errors"
	"time"
)

var ErrNotImplemented = errors.New("pubsub adapter not implemented")

type RetryPolicy struct {
	MaxRetries int
	Backoff    time.Duration
	MaxBackoff time.Duration
	DLQTopic   string
}

type Message struct {
	ID       string
	Topic    string
	Payload  []byte
	Headers  map[string]string
	Attempts int
}

type Adapter interface {
	Connect(ctx context.Context) error
	Subscribe(ctx context.Context, topic, group string) (<-chan *Message, error)
	Publish(ctx context.Context, topic string, msg *Message) error
	Ack(msg *Message) error
	Nack(msg *Message, retry RetryPolicy) error
	Close() error
}

type Config struct {
	BrokerType string
	Endpoint   string
}

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

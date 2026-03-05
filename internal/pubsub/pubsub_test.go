package pubsub

import (
	"context"
	"testing"
	"time"
)

func TestDedupStore_SeenBefore(t *testing.T) {
	d := NewDedupStore(20 * time.Millisecond)
	now := time.Now()
	if d.SeenBefore("k1", now) {
		t.Fatal("first message must not be duplicate")
	}
	d.Mark("k1", now)
	if !d.SeenBefore("k1", now.Add(5*time.Millisecond)) {
		t.Fatal("second message in ttl should be duplicate")
	}
	if d.SeenBefore("k1", now.Add(30*time.Millisecond)) {
		t.Fatal("message after ttl should be accepted")
	}
}

func TestRetryPolicy(t *testing.T) {
	p := RetryPolicy{MaxRetries: 3, Backoff: 10 * time.Millisecond, MaxBackoff: 25 * time.Millisecond}
	if !ShouldRetry(p, 1) || !ShouldRetry(p, 3) || ShouldRetry(p, 4) {
		t.Fatal("retry decision mismatch")
	}
	if got := NextBackoff(p, 1); got != 10*time.Millisecond {
		t.Fatalf("attempt1 backoff mismatch: %v", got)
	}
	if got := NextBackoff(p, 3); got != 25*time.Millisecond {
		t.Fatalf("attempt3 capped backoff mismatch: %v", got)
	}
}

func TestAdapterContract_NATSAndRedis(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		cfg  Config
	}{
		{name: "nats", cfg: Config{BrokerType: "nats", Endpoint: "test-contract"}},
		{name: "redis_streams", cfg: Config{BrokerType: "redis_streams", Endpoint: "test-contract"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := NewAdapter(c.cfg)
			if err := a.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			ch, err := a.Subscribe(ctx, "topic.in", "g1")
			if err != nil {
				t.Fatal(err)
			}
			msg := &Message{ID: "m1", Payload: []byte("hello"), Headers: map[string]string{"k": "v"}}
			if err := a.Publish(ctx, "topic.in", msg); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-ch:
				if got.ID != "m1" {
					t.Fatalf("unexpected id: %s", got.ID)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatal("timeout waiting published msg")
			}
			if err := a.Ack(msg); err != nil {
				t.Fatal(err)
			}
			if err := a.Nack(msg, RetryPolicy{MaxRetries: 1}); err != nil {
				t.Fatal(err)
			}
			if msg.Attempts != 1 {
				t.Fatalf("expected attempts=1, got %d", msg.Attempts)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSolaceAdapter_Scaffold(t *testing.T) {
	a := NewAdapter(Config{BrokerType: "solace"})
	if err := a.Connect(context.Background()); err == nil {
		t.Fatal("expected not implemented error")
	}
	if _, err := a.Subscribe(context.Background(), "t", "g"); err == nil {
		t.Fatal("expected subscribe not implemented")
	}
	if err := a.Publish(context.Background(), "t", &Message{ID: "x"}); err == nil {
		t.Fatal("expected publish not implemented")
	}
	if err := a.Ack(&Message{ID: "x"}); err == nil {
		t.Fatal("expected ack not implemented")
	}
	if err := a.Nack(&Message{ID: "x"}, RetryPolicy{}); err == nil {
		t.Fatal("expected nack not implemented")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInMemoryBroker_FailureInjection(t *testing.T) {
	b := NewInMemoryBroker()
	b.FailNextPublish("f-topic", 1)
	if err := b.Publish(context.Background(), "f-topic", &Message{ID: "x"}); err == nil {
		t.Fatal("expected injected publish failure")
	}
	if err := b.Publish(context.Background(), "f-topic", &Message{ID: "y"}); err != nil {
		t.Fatal(err)
	}
}

func TestAdapterErrors_Validation(t *testing.T) {
	a := NewAdapter(Config{BrokerType: "nats", Endpoint: "err"})
	if _, err := a.Subscribe(context.Background(), "", "g"); err == nil {
		t.Fatal("expected subscribe validation error")
	}
	if err := a.Publish(context.Background(), "", &Message{ID: "x"}); err == nil {
		t.Fatal("expected publish topic validation error")
	}
	if err := a.Publish(context.Background(), "topic", nil); err == nil {
		t.Fatal("expected publish message validation error")
	}
	if err := a.Nack(nil, RetryPolicy{}); err == nil {
		t.Fatal("expected nack message validation error")
	}
	if got := NewAdapter(Config{BrokerType: "unknown"}); got == nil {
		t.Fatal("expected fallback adapter")
	}
}

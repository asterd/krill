package bus

import (
	"context"
	"testing"
	"time"

	"github.com/krill/krill/internal/telemetry"
)

func TestLocalBusPublishSubscribeAndUnsubscribe(t *testing.T) {
	telemetry.Configure(telemetry.Config{Profile: "off", Exporter: "none"}, nil)
	defer telemetry.Shutdown(context.Background())

	b := NewLocal(2)
	ch := b.Subscribe("k")
	env := &Envelope{
		ID:        "1",
		ClientID:  "c1",
		ThreadID:  "t1",
		Role:      RoleUser,
		Text:      "hello",
		CreatedAt: time.Now().UTC(),
	}
	if err := b.Publish(context.Background(), "k", env); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if got.ID != "1" {
			t.Fatalf("unexpected id: %s", got.ID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting published message")
	}
	b.Unsubscribe("k")
	b.Unsubscribe("k")
}

func TestLocalBusPublishContextCancelAndBackpressure(t *testing.T) {
	SetReplyPrefix("__reply__")
	b := NewLocal(1)
	ch := b.Subscribe("x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = b.Publish(ctx, "x", &Envelope{})

	okCtx := context.Background()
	if err := b.Publish(okCtx, "x", &Envelope{ID: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(okCtx, "x", &Envelope{ID: "2"}); err != ErrBackpressure {
		t.Fatalf("expected backpressure, got %v", err)
	}
	select {
	case got := <-ch:
		if got.ID != "1" {
			t.Fatalf("expected first message to remain queued, got %s", got.ID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout")
	}

	if ReplyKey("http") == ReplyKey("telegram") {
		t.Fatal("reply keys must differ by protocol")
	}
}

func TestReplyKey_UsesConfiguredPrefix(t *testing.T) {
	SetReplyPrefix("custom-reply")
	defer SetReplyPrefix("__reply__")
	if got := ReplyKey("http"); got != "custom-reply:http" {
		t.Fatalf("unexpected reply key: %s", got)
	}
}

func TestLocalBusPublish_CanReturnContextError(t *testing.T) {
	b := NewLocal(1)
	_ = b.Subscribe("k")
	_ = b.Publish(context.Background(), "k", &Envelope{ID: "warmup"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gotErr := false
	for i := 0; i < 200; i++ {
		if err := b.Publish(ctx, "k", &Envelope{ID: "x"}); err != nil {
			gotErr = true
			break
		}
	}
	if !gotErr {
		t.Fatal("expected at least one context canceled error from publish")
	}
}

func TestLocalBackbone_SubscribeQueueIsolationAndClose(t *testing.T) {
	backbone, ok := NewLocal(2).(Backbone)
	if !ok {
		t.Fatal("expected Backbone implementation")
	}
	subA := backbone.SubscribeQueue("topic")
	subB := backbone.SubscribeQueue("topic")
	if backbone.SubscriberCount("topic") != 2 {
		t.Fatalf("expected 2 subscribers, got %d", backbone.SubscriberCount("topic"))
	}

	env := &Envelope{ID: "1", CreatedAt: time.Now().UTC()}
	if err := backbone.Publish(context.Background(), "topic", env); err != nil {
		t.Fatal(err)
	}
	if got := <-subA.C(); got.ID != "1" {
		t.Fatalf("unexpected subA env: %+v", got)
	}
	if got := <-subB.C(); got.ID != "1" {
		t.Fatalf("unexpected subB env: %+v", got)
	}
	subA.Ack(env)
	subA.Close()
	if backbone.SubscriberCount("topic") != 1 {
		t.Fatalf("expected 1 subscriber after close, got %d", backbone.SubscriberCount("topic"))
	}
	select {
	case _, ok := <-subA.C():
		if ok {
			t.Fatal("expected closed channel")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting closed channel")
	}
	backbone.Unsubscribe("topic")
	if backbone.SubscriberCount("topic") != 0 {
		t.Fatalf("expected no subscribers after unsubscribe, got %d", backbone.SubscriberCount("topic"))
	}
}

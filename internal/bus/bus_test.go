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

func TestLocalBusPublishContextCancelAndDropOldest(t *testing.T) {
	b := NewLocal(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = b.Publish(ctx, "x", &Envelope{})

	okCtx := context.Background()
	_ = b.Publish(okCtx, "x", &Envelope{ID: "1"})
	_ = b.Publish(okCtx, "x", &Envelope{ID: "2"})
	ch := b.Subscribe("x")
	select {
	case got := <-ch:
		if got.ID != "2" {
			t.Fatalf("expected newest message after drop-oldest, got %s", got.ID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout")
	}

	if ReplyKey("http") == ReplyKey("telegram") {
		t.Fatal("reply keys must differ by protocol")
	}
}

func TestLocalBusPublish_CanReturnContextError(t *testing.T) {
	b := NewLocal(1)
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

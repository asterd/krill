package ingress

import (
	"context"
	"testing"
	"time"

	"github.com/krill/krill/internal/bus"
)

func TestPublishInbound_ProtocolCompatibility(t *testing.T) {
	n := NewNormalizer(false)
	b := bus.NewLocal(8)
	ch := b.Subscribe(bus.InboundKey)

	cases := []struct {
		name     string
		protocol string
		clientID string
		metaKey  string
		metaVal  string
	}{
		{name: "http", protocol: "http", clientID: "http:c1", metaKey: "request_id", metaVal: "r-1"},
		{name: "telegram", protocol: "telegram", clientID: "tg:99", metaKey: "tg_chat_id", metaVal: "99"},
		{name: "webhook", protocol: "webhook", clientID: "wh:abc", metaKey: "raw", metaVal: "{\"x\":1}"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			in := &bus.Envelope{
				ID:             "id-" + tc.name,
				ClientID:       tc.clientID,
				ThreadID:       tc.clientID,
				Role:           bus.RoleUser,
				Text:           "hello-" + tc.name,
				SourceProtocol: tc.protocol,
				Meta:           map[string]string{tc.metaKey: tc.metaVal},
				CreatedAt:      time.Date(2026, 3, 5, 10, 0, 0, 0, time.UTC),
			}
			if err := n.PublishInbound(context.Background(), b, in); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-ch:
				if got.ClientID != in.ClientID || got.Text != in.Text || got.SourceProtocol != in.SourceProtocol {
					t.Fatalf("regression: inbound changed: in=%+v got=%+v", in, got)
				}
				if got.Meta[tc.metaKey] != tc.metaVal {
					t.Fatalf("meta mismatch for %s: %q", tc.metaKey, got.Meta[tc.metaKey])
				}
				if got.Meta["schema_version"] != "v2" {
					t.Fatalf("schema_version metadata missing: %+v", got.Meta)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatal("timeout waiting normalized envelope")
			}
		})
	}
}

func TestNormalizeInbound_StrictRejectsMissingSourceProtocol(t *testing.T) {
	n := NewNormalizer(true)
	_, err := n.NormalizeInbound(&bus.Envelope{
		ID:        "1",
		ClientID:  "c1",
		ThreadID:  "t1",
		Role:      bus.RoleUser,
		Text:      "x",
		CreatedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestPublishInbound_ContextCancelled(t *testing.T) {
	n := NewNormalizer(false)
	b := bus.NewLocal(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := n.PublishInbound(ctx, b, &bus.Envelope{
		ID:             "1",
		ClientID:       "c1",
		ThreadID:       "t1",
		Role:           bus.RoleUser,
		Text:           "x",
		SourceProtocol: "http",
		CreatedAt:      time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected publish error when context cancelled")
	}
}

func TestPublishInbound_NormalizeError(t *testing.T) {
	n := NewNormalizer(true)
	b := bus.NewLocal(1)
	err := n.PublishInbound(context.Background(), b, &bus.Envelope{
		ID:        "1",
		ClientID:  "c1",
		ThreadID:  "t1",
		Role:      bus.RoleUser,
		Text:      "x",
		CreatedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("expected normalization error")
	}
}

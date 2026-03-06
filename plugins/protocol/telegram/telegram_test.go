package telegram

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/plugincfg"
)

type telegramRT struct {
	mu       sync.Mutex
	updates  int
	sends    int
	lastBody string
}

func (rt *telegramRT) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	switch {
	case strings.Contains(req.URL.Path, "/getUpdates"):
		rt.updates++
		body := `{"ok":true,"result":[{"update_id":1,"message":{"text":"ciao","chat":{"id":42},"from":{"username":"tester"}}}]}`
		if rt.updates > 1 {
			body = `{"ok":true,"result":[]}`
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	case strings.Contains(req.URL.Path, "/sendMessage"):
		rt.sends++
		b, _ := io.ReadAll(req.Body)
		rt.lastBody = string(b)
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Header:     make(http.Header),
		}, nil
	default:
		return &http.Response{
			StatusCode: 404,
			Body:       io.NopCloser(bytes.NewBufferString(`{"ok":false}`)),
			Header:     make(http.Header),
		}, nil
	}
}

type errorRT struct{}

func (errorRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network down")
}

type telegramBadStatusRT struct{}

func (telegramBadStatusRT) RoundTrip(req *http.Request) (*http.Response, error) {
	body := `{"ok":false}`
	return &http.Response{
		StatusCode: 500,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

type telegramBadAckRT struct{}

func (telegramBadAckRT) RoundTrip(req *http.Request) (*http.Response, error) {
	body := `{"ok":false}`
	if strings.Contains(req.URL.Path, "/getUpdates") {
		body = `{"ok":false,"result":[]}`
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func TestNewStartPollRelayStop_NonRegression(t *testing.T) {
	p, err := New(map[string]interface{}{"token": "test-token", "poll_ms": 1, "_strict_v2_validation": true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "telegram" {
		t.Fatalf("unexpected name: %s", p.Name())
	}
	rt := &telegramRT{}
	p.http = &http.Client{Transport: rt}

	b := bus.NewLocal(16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx, b, slog.Default()); err != nil {
		t.Fatal(err)
	}

	select {
	case env := <-b.Subscribe(bus.InboundKey):
		if env.SourceProtocol != "telegram" || env.ClientID != "tg:42" || env.Text != "ciao" {
			t.Fatalf("unexpected poll env: %+v", env)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting telegram poll message")
	}

	if err := b.Publish(context.Background(), bus.ReplyKey("telegram"), &bus.Envelope{
		ID:             "r1",
		ClientID:       "tg:42",
		Role:           bus.RoleAssistant,
		Text:           "pong",
		SourceProtocol: "telegram",
		Meta:           map[string]string{"tg_chat_id": "42"},
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.updates == 0 || rt.sends == 0 {
		t.Fatalf("expected getUpdates and sendMessage calls, updates=%d sends=%d", rt.updates, rt.sends)
	}
	if !strings.Contains(rt.lastBody, `"chat_id":"42"`) {
		t.Fatalf("unexpected send body: %s", rt.lastBody)
	}
}

func TestGetUpdatesSendAndHelpers(t *testing.T) {
	p, err := New(map[string]interface{}{"token": "token-1", "poll_ms": 1000})
	if err != nil {
		t.Fatal(err)
	}
	rt := &telegramRT{}
	p.http = &http.Client{Transport: rt}

	upd, err := p.getUpdates(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(upd) != 1 || upd[0].Message.Text != "ciao" {
		t.Fatalf("unexpected updates: %+v", upd)
	}
	if err := p.send(context.Background(), "42", "hello"); err != nil {
		t.Fatal(err)
	}
	if !plugincfg.Bool(map[string]interface{}{"v": "true"}, "v") {
		t.Fatal("boolVal should parse true string")
	}
	if plugincfg.Bool(map[string]interface{}{"v": "false"}, "v") {
		t.Fatal("boolVal false branch should be false")
	}
}

func TestNew_RequiresToken(t *testing.T) {
	if _, err := New(map[string]interface{}{}); err == nil {
		t.Fatal("expected token required error")
	}
}

func TestRelay_FallbackChatIDFromClientID(t *testing.T) {
	p, _ := New(map[string]interface{}{"token": "t", "poll_ms": 1000})
	rt := &telegramRT{}
	p.http = &http.Client{Transport: rt}
	p.b = bus.NewLocal(4)
	p.log = slog.Default()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.relay(ctx)
	time.Sleep(10 * time.Millisecond)
	_ = p.b.Publish(context.Background(), bus.ReplyKey("telegram"), &bus.Envelope{
		ID:             "r1",
		ClientID:       "tg:777",
		Role:           bus.RoleAssistant,
		Text:           "ok",
		SourceProtocol: "telegram",
		CreatedAt:      time.Now().UTC(),
	})
	time.Sleep(50 * time.Millisecond)
	cancel()

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.sends == 0 {
		t.Fatal("expected relay to call send with fallback chat_id")
	}
}

func TestGetUpdatesAndSend_ErrorBranches(t *testing.T) {
	p, _ := New(map[string]interface{}{"token": "t", "poll_ms": 1000})
	p.http = &http.Client{Transport: errorRT{}}
	if _, err := p.getUpdates(context.Background(), 0); err == nil {
		t.Fatal("expected getUpdates transport error")
	}
	if err := p.send(context.Background(), "42", "x"); err == nil {
		t.Fatal("expected send transport error")
	}

	p.http = &http.Client{Transport: telegramBadStatusRT{}}
	if _, err := p.getUpdates(context.Background(), 0); err == nil {
		t.Fatal("expected getUpdates http status error")
	}
	if err := p.send(context.Background(), "42", "x"); err == nil {
		t.Fatal("expected send http status error")
	}

	p.http = &http.Client{Transport: telegramBadAckRT{}}
	if _, err := p.getUpdates(context.Background(), 0); err == nil {
		t.Fatal("expected getUpdates ok=false error")
	}
	if err := p.send(context.Background(), "42", "x"); err == nil {
		t.Fatal("expected send ok=false error")
	}
}

func TestRelay_IgnoresNonAssistantAndMissingChat(t *testing.T) {
	p, _ := New(map[string]interface{}{"token": "t", "poll_ms": 1000})
	rt := &telegramRT{}
	p.http = &http.Client{Transport: rt}
	p.b = bus.NewLocal(8)
	p.log = slog.Default()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.relay(ctx)
	time.Sleep(10 * time.Millisecond)
	_ = p.b.Publish(context.Background(), bus.ReplyKey("telegram"), &bus.Envelope{
		ID:        "1",
		ClientID:  "x",
		Role:      bus.RoleUser,
		CreatedAt: time.Now().UTC(),
	})
	_ = p.b.Publish(context.Background(), bus.ReplyKey("telegram"), &bus.Envelope{
		ID:        "2",
		ClientID:  "x",
		Role:      bus.RoleAssistant,
		Text:      "assistant",
		CreatedAt: time.Now().UTC(),
	})
	time.Sleep(50 * time.Millisecond)
	cancel()

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.sends != 0 {
		t.Fatalf("expected no send for non-assistant/missing chat, got %d", rt.sends)
	}
}

package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/plugincfg"
)

func TestHandle_NonRegressionFlow(t *testing.T) {
	p, err := New(map[string]interface{}{
		"path":         "/webhook",
		"client_field": "user.id",
		"text_field":   "text",
	})
	if err != nil {
		t.Fatal(err)
	}
	b := bus.NewLocal(8)
	p.b = b
	p.log = slog.Default()

	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(`{"user":{"id":"u-1"},"text":"hello-webhook"}`))
	w := httptest.NewRecorder()
	p.handle(w, req)

	if w.Result().StatusCode != 202 {
		t.Fatalf("expected status 202, got %d", w.Result().StatusCode)
	}

	select {
	case env := <-b.Subscribe(bus.InboundKey):
		if env.SourceProtocol != "webhook" || env.ClientID != "u-1" || env.Text != "hello-webhook" {
			t.Fatalf("unexpected env: %+v", env)
		}
		if env.Meta["schema_version"] != "v2" {
			t.Fatalf("expected normalized v2 metadata, got %+v", env.Meta)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting inbound env")
	}
}

func TestRelay_IgnoresNonAssistant(t *testing.T) {
	p, _ := New(map[string]interface{}{})
	b := bus.NewLocal(4)
	p.b = b
	p.log = slog.Default()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.relay(ctx)
		close(done)
	}()

	_ = b.Publish(context.Background(), bus.ReplyKey("webhook"), &bus.Envelope{
		ID:        "x",
		ClientID:  "c1",
		Role:      bus.RoleUser,
		CreatedAt: time.Now().UTC(),
	})
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("relay did not stop on context cancel")
	}
}

func TestRelay_ReturnsWhenChannelClosed(t *testing.T) {
	p, _ := New(map[string]interface{}{})
	b := bus.NewLocal(4)
	p.b = b
	p.log = slog.Default()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.relay(ctx)
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	b.Unsubscribe(bus.ReplyKey("webhook"))
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("relay did not stop when reply channel was closed")
	}
}

func TestHandle_VerificationBranches(t *testing.T) {
	payload := `{"user":{"id":"u-2"},"text":"x","type":"url_verification","challenge":"abc"}`
	p, _ := New(map[string]interface{}{"verify_mode": "none"})
	p.b = bus.NewLocal(4)
	p.log = slog.Default()
	w := httptest.NewRecorder()
	p.handle(w, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "abc") {
		t.Fatalf("slack verification branch failed: code=%d body=%s", w.Code, w.Body.String())
	}

	p2, _ := New(map[string]interface{}{"verify_mode": "discord"})
	p2.b = bus.NewLocal(4)
	p2.log = slog.Default()
	w2 := httptest.NewRecorder()
	p2.handle(w2, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"type":1}`)))
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), `"type":1`) {
		t.Fatalf("discord verification branch failed: code=%d body=%s", w2.Code, w2.Body.String())
	}
}

func TestHMACAndStartStopHelpers(t *testing.T) {
	secret := "topsecret"
	body := []byte(`{"x":1}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !verifyHMAC(body, secret, sig) {
		t.Fatal("verifyHMAC expected true")
	}
	if verifyHMAC(body, secret, "sha256=deadbeef") {
		t.Fatal("verifyHMAC expected false on wrong signature")
	}
	if !plugincfg.Bool(map[string]interface{}{"x": "true"}, "x") {
		t.Fatal("boolVal expected true")
	}
	if got := dotGet(map[string]interface{}{"a": map[string]interface{}{"b": "c"}}, "a.b"); got != "c" {
		t.Fatalf("dotGet mismatch: %s", got)
	}
	if got := plugincfg.StringDefault(map[string]interface{}{}, "missing", "d"); got != "d" {
		t.Fatalf("str fallback mismatch: %s", got)
	}
	if plugincfg.Bool(map[string]interface{}{"x": "false"}, "x") {
		t.Fatal("boolVal false branch expected false")
	}
	if got := dotGet(map[string]interface{}{"a": "x"}, "a.b"); got != "" {
		t.Fatalf("dotGet non-map branch expected empty, got %s", got)
	}

	p, _ := New(map[string]interface{}{"addr": "127.0.0.1:0", "path": "/webhook"})
	if p.Name() != "webhook" {
		t.Fatalf("unexpected name: %s", p.Name())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx, bus.NewLocal(4), slog.Default()); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHandle_ErrorBranches(t *testing.T) {
	p, _ := New(map[string]interface{}{"secret": "k", "verify_mode": "hmac"})
	p.b = bus.NewLocal(4)
	p.log = slog.Default()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"x":1}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=bad")
	p.handle(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized for bad hmac, got %d", w.Code)
	}

	p2, _ := New(map[string]interface{}{})
	p2.b = bus.NewLocal(4)
	p2.log = slog.Default()
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`not-json`))
	p2.handle(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request invalid json, got %d", w2.Code)
	}
}

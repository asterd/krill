package httpproto

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krill/krill/internal/bus"
)

func TestHandleChat_NonRegressionFlow(t *testing.T) {
	p, err := New(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	b := bus.NewLocal(16)
	p.b = b
	p.log = slog.Default()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.relayReplies(ctx)

	inbound := b.Subscribe(bus.InboundKey)
	go func() {
		env := <-inbound
		if env.SourceProtocol != "http" || env.Role != bus.RoleUser {
			t.Errorf("unexpected inbound envelope: %+v", env)
			return
		}
		_ = b.Publish(context.Background(), bus.ReplyKey("http"), &bus.Envelope{
			ID:             "r1",
			ClientID:       env.ClientID,
			ThreadID:       env.ThreadID,
			Role:           bus.RoleAssistant,
			Text:           "pong",
			SourceProtocol: "http",
			Meta:           map[string]string{"agent": "test-agent", "request_id": env.Meta["request_id"]},
			CreatedAt:      time.Now().UTC(),
		})
	}()

	req := httptest.NewRequest("POST", "/v1/chat", strings.NewReader(`{"client_id":"c1","thread_id":"t1","message":"ping"}`))
	w := httptest.NewRecorder()
	p.handleChat(w, req)
	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"text":"pong"`) {
		t.Fatalf("expected SSE response with pong, got %s", body)
	}
}

func TestHandleHealthAndAuthAndTrace(t *testing.T) {
	p, _ := New(map[string]interface{}{"api_key": "secret"})

	w := httptest.NewRecorder()
	handleHealth(w, httptest.NewRequest("GET", "/v1/health", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("health status=%d", w.Result().StatusCode)
	}

	authorized := false
	protected := p.auth(func(_ http.ResponseWriter, _ *http.Request) { authorized = true })
	req := httptest.NewRequest("GET", "/v1/chat", nil)
	protected(httptest.NewRecorder(), req)
	if authorized {
		t.Fatal("expected unauthorized request to be blocked")
	}
	req.Header.Set("Authorization", "Bearer secret")
	protected(httptest.NewRecorder(), req)
	if !authorized {
		t.Fatal("expected authorized request to pass")
	}

	if traceIDFromRequest(httptest.NewRequest("GET", "/", nil)) == "" {
		t.Fatal("expected generated trace id")
	}
	r1 := httptest.NewRequest("GET", "/", nil)
	r1.Header.Set("X-Trace-ID", "trace-header")
	if got := traceIDFromRequest(r1); got != "trace-header" {
		t.Fatalf("trace id from header mismatch: %s", got)
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("traceparent", "00-1234567890abcdef1234567890abcdef-0123456789abcdef-01")
	if got := traceIDFromRequest(r2); got != "1234567890abcdef1234567890abcdef" {
		t.Fatalf("trace id from traceparent mismatch: %s", got)
	}
}

func TestPluginStartStopAndHelpers(t *testing.T) {
	p, err := New(map[string]interface{}{"addr": "127.0.0.1:0", "_strict_v2_validation": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "http" {
		t.Fatalf("unexpected name: %s", p.Name())
	}
	if !boolVal(map[string]interface{}{"x": true}, "x") {
		t.Fatal("boolVal bool branch failed")
	}
	if !boolVal(map[string]interface{}{"x": "true"}, "x") {
		t.Fatal("boolVal string branch failed")
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

func TestHandleChat_ErrorBranchesAndRelayIgnore(t *testing.T) {
	p, _ := New(map[string]interface{}{})
	p.b = bus.NewLocal(4)
	p.log = slog.Default()

	req := httptest.NewRequest("GET", "/v1/chat", nil)
	w := httptest.NewRecorder()
	p.handleChat(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}

	req = httptest.NewRequest("POST", "/v1/chat", strings.NewReader("{"))
	w = httptest.NewRecorder()
	p.handleChat(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected bad json 400, got %d", w.Code)
	}

	req = httptest.NewRequest("POST", "/v1/chat", strings.NewReader(`{"client_id":"c1","message":""}`))
	w = httptest.NewRecorder()
	p.handleChat(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected message required 400, got %d", w.Code)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.relayReplies(ctx)
	_ = p.b.Publish(context.Background(), bus.ReplyKey("http"), &bus.Envelope{
		ID:             "u1",
		ClientID:       "c1",
		Role:           bus.RoleUser,
		SourceProtocol: "http",
		Meta:           map[string]string{"request_id": "ignored"},
		CreatedAt:      time.Now().UTC(),
	})
}

func TestHandleChat_ConcurrentRequestsSameClientUseRequestCorrelation(t *testing.T) {
	p, err := New(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	b := bus.NewLocal(16)
	p.b = b
	p.log = slog.Default()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.relayReplies(ctx)

	inbound := b.Subscribe(bus.InboundKey)
	go func() {
		first := <-inbound
		second := <-inbound
		_ = b.Publish(context.Background(), bus.ReplyKey("http"), &bus.Envelope{
			ID:             "r-second",
			ClientID:       second.ClientID,
			ThreadID:       second.ThreadID,
			Role:           bus.RoleAssistant,
			Text:           "second",
			SourceProtocol: "http",
			Meta:           map[string]string{"request_id": second.Meta["request_id"], "agent": "test"},
			CreatedAt:      time.Now().UTC(),
		})
		_ = b.Publish(context.Background(), bus.ReplyKey("http"), &bus.Envelope{
			ID:             "r-first",
			ClientID:       first.ClientID,
			ThreadID:       first.ThreadID,
			Role:           bus.RoleAssistant,
			Text:           "first",
			SourceProtocol: "http",
			Meta:           map[string]string{"request_id": first.Meta["request_id"], "agent": "test"},
			CreatedAt:      time.Now().UTC(),
		})
	}()

	results := make(chan string, 2)
	run := func(msg string) {
		req := httptest.NewRequest("POST", "/v1/chat", strings.NewReader(`{"client_id":"c1","thread_id":"t1","message":"`+msg+`"}`))
		w := httptest.NewRecorder()
		p.handleChat(w, req)
		results <- w.Body.String()
	}
	go run("first")
	go run("second")

	got1 := <-results
	got2 := <-results
	if !strings.Contains(got1, `"text":"first"`) && !strings.Contains(got1, `"text":"second"`) {
		t.Fatalf("unexpected first response body %s", got1)
	}
	if !strings.Contains(got2, `"text":"first"`) && !strings.Contains(got2, `"text":"second"`) {
		t.Fatalf("unexpected second response body %s", got2)
	}
	if got1 == got2 {
		t.Fatalf("expected distinct correlated responses, got %q and %q", got1, got2)
	}
}

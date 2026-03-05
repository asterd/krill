package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/plugincfg"
)

func TestNewRejectsInvalidPath(t *testing.T) {
	if _, err := New(map[string]interface{}{"path": "bad"}); err == nil {
		t.Fatal("expected invalid path error")
	}
	p, err := New(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "a2a" {
		t.Fatalf("unexpected plugin name: %s", p.Name())
	}
}

func TestA2AHandleEnvelopeStrictValidationAndMetadata(t *testing.T) {
	p, err := New(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	b := bus.NewLocal(4)
	p.b = b
	p.log = slog.New(slog.NewTextHandler(io.Discard, nil))

	badReq, _ := http.NewRequest(http.MethodPost, p.path, bytes.NewBufferString(`{"schema_version":"v2"}`))
	badRec := newRecorder()
	p.handleEnvelope(badRec, badReq)
	if badRec.code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", badRec.code)
	}

	payload := map[string]any{
		"schema_version":  "v2",
		"id":              "a2a-1",
		"client_id":       "c1",
		"thread_id":       "t1",
		"tenant":          "default",
		"workflow_id":     "wf-coop",
		"source_protocol": "a2a",
		"role":            "user",
		"text":            "handoff input",
		"created_at":      "2026-03-05T10:00:00Z",
		"meta": map[string]string{
			"origin_agent":   "router",
			"target_agent":   "specialist",
			"handoff_reason": "need specialization",
		},
	}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, p.path, bytes.NewBuffer(data))
	req.Header.Set("traceparent", "00-1234567890abcdef1234567890abcdef-1234567890abcdef-01")
	rec := newRecorder()
	p.handleEnvelope(rec, req)
	if rec.code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.code)
	}

	select {
	case env := <-b.Subscribe(bus.InboundKey):
		if env.SourceProtocol != "a2a" {
			t.Fatalf("unexpected source protocol: %s", env.SourceProtocol)
		}
		if env.Meta["origin_agent"] != "router" || env.Meta["target_agent"] != "specialist" {
			t.Fatalf("handoff metadata not preserved: %+v", env.Meta)
		}
		if env.Meta["trace_id"] == "" {
			t.Fatal("expected trace_id")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting inbound envelope")
	}
}

func TestA2AHandleEnvelopeMethodAndHeaderFallback(t *testing.T) {
	p, err := New(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	b := bus.NewLocal(4)
	p.b = b
	p.log = slog.New(slog.NewTextHandler(io.Discard, nil))

	req, _ := http.NewRequest(http.MethodGet, p.path, nil)
	rec := newRecorder()
	p.handleEnvelope(rec, req)
	if rec.code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.code)
	}

	payload := map[string]any{
		"schema_version":  "v2",
		"id":              "a2a-2",
		"client_id":       "c2",
		"thread_id":       "t2",
		"tenant":          "default",
		"workflow_id":     "wf-coop",
		"source_protocol": "a2a",
		"role":            "user",
		"text":            "handoff input",
		"created_at":      "2026-03-05T10:00:00Z",
	}
	data, _ := json.Marshal(payload)
	req2, _ := http.NewRequest(http.MethodPost, p.path, bytes.NewBuffer(data))
	req2.Header.Set("X-A2A-Origin-Agent", "r1")
	req2.Header.Set("X-A2A-Target-Agent", "s1")
	req2.Header.Set("X-A2A-Handoff-Reason", "fallback")
	rec2 := newRecorder()
	p.handleEnvelope(rec2, req2)
	if rec2.code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec2.code)
	}
	select {
	case env := <-b.Subscribe(bus.InboundKey):
		if env.Meta["origin_agent"] != "r1" || env.Meta["target_agent"] != "s1" || env.Meta["handoff_reason"] != "fallback" {
			t.Fatalf("expected header fallback handoff metadata, got %+v", env.Meta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting inbound envelope")
	}
}

func TestA2APluginLifecycle(t *testing.T) {
	p, err := New(map[string]interface{}{"addr": ":18091"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx, bus.NewLocal(4), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	p2, err := New(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p2.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRelayReplies(t *testing.T) {
	p, err := New(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	p.b = bus.NewLocal(4)
	p.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.relayReplies(ctx)
	_ = p.b.Publish(context.Background(), bus.ReplyKey("a2a"), &bus.Envelope{Role: bus.RoleSystem, ClientID: "c1"})
	_ = p.b.Publish(context.Background(), bus.ReplyKey("a2a"), &bus.Envelope{Role: bus.RoleAssistant, ClientID: "c1", Text: "ok", Meta: map[string]string{"workflow_id": "w"}})
	time.Sleep(20 * time.Millisecond)
}

func TestHelpers(t *testing.T) {
	if got := plugincfg.String(nil, "k"); got != "" {
		t.Fatalf("expected empty strVal for nil map, got %q", got)
	}
	if got := plugincfg.String(map[string]interface{}{"k": " v "}, "k"); got != "v" {
		t.Fatalf("unexpected strVal: %q", got)
	}
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Test", " value ")
	if got := strHeader(req, "X-Test"); got != "value" {
		t.Fatalf("unexpected strHeader: %q", got)
	}
	if got := extractTraceID(req); got == "" {
		t.Fatal("expected generated trace id")
	}
}

type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func newRecorder() *recorder {
	return &recorder{header: make(http.Header), code: http.StatusOK}
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(statusCode int)  { r.code = statusCode }
func (r *recorder) Write(p []byte) (int, error) { return r.body.Write(p) }

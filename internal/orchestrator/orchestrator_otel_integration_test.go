package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/ingress"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/skill"
	"github.com/krill/krill/internal/telemetry"
)

func TestIntegration_TraceCorrelationIngressToReply(t *testing.T) {
	telemetry.Configure(telemetry.Config{Profile: "debug", Exporter: "none", ServiceName: "krill-test"}, slog.Default())
	defer telemetry.Shutdown(context.Background())

	var mockLLM *httptest.Server
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				if strings.Contains(toPanicString(recovered), "operation not permitted") {
					t.Skip("local listener not permitted in current sandbox")
				}
				panic(recovered)
			}
		}()
		mockLLM = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"message": map[string]any{
							"role":    "assistant",
							"content": "ok",
						},
					},
				},
				"usage": map[string]any{
					"prompt_tokens":     1,
					"completion_tokens": 1,
					"total_tokens":      2,
				},
			})
		}))
	}()
	defer mockLLM.Close()

	cfg := &config.Root{
		Core: config.CoreConfig{
			BusBuffer:      64,
			MaxClients:     4,
			SandboxType:    "exec",
			SkillTimeoutMs: 1000,
			MemoryBackend:  "ram",
			MemoryWindow:   50,
		},
		LLM: config.LLMPool{
			Default: "mock",
			Backends: []config.LLMConfig{
				{
					Name:      "mock",
					BaseURL:   mockLLM.URL,
					APIKey:    "test",
					Model:     "mock-model",
					MaxTokens: 64,
				},
			},
		},
		Agents: []config.AgentConfig{
			{
				Name:         "default",
				LLM:          "mock",
				MaxTurns:     2,
				SystemPrompt: "You are test agent.",
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := bus.NewLocal(64)
	mem := memory.NewRAM()
	skillReg, err := skill.NewRegistry(nil, cfg.Core, logger)
	if err != nil {
		t.Fatal(err)
	}
	llmPool, err := llm.NewPool(cfg.LLM)
	if err != nil {
		t.Fatal(err)
	}
	orch, err := New(cfg, b, skillReg, mem, llmPool, logger, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := orch.Start(ctx); err != nil {
		t.Fatal(err)
	}

	replyCh := b.Subscribe(bus.ReplyKey("http"))
	norm := ingress.NewNormalizer(false)
	const (
		traceID   = "trace-e2e-123"
		requestID = "req-e2e-456"
	)
	err = norm.PublishInbound(ctx, b, &bus.Envelope{
		ID:             "env-1",
		ClientID:       "http:c1",
		ThreadID:       "http:c1",
		Role:           bus.RoleUser,
		Text:           "hello",
		SourceProtocol: "http",
		Meta: map[string]string{
			"trace_id":     traceID,
			"request_id":   requestID,
			"ingress_span": "ing-1",
		},
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case reply := <-replyCh:
		if reply.Meta["trace_id"] != traceID {
			t.Fatalf("trace correlation lost: got %q", reply.Meta["trace_id"])
		}
		if reply.Meta["request_id"] != requestID {
			t.Fatalf("request correlation lost: got %q", reply.Meta["request_id"])
		}
		if reply.Text == "" {
			t.Fatalf("expected assistant reply")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting correlated reply")
	}
}

func toPanicString(v any) string {
	if err, ok := v.(error); ok {
		return err.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

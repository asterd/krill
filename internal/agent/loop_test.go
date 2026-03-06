package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/skill"
	"github.com/krill/krill/internal/telemetry"
)

type echoExec struct{}

func (echoExec) Execute(_ context.Context, argsJSON string) (string, error) {
	return "echo:" + argsJSON, nil
}

func newTestLoop(t *testing.T, handler http.HandlerFunc, eagerSkills ...string) (*Loop, memory.Store, <-chan *bus.Envelope) {
	t.Helper()
	telemetry.Configure(telemetry.Config{Profile: "off", Exporter: "none"}, nil)
	t.Cleanup(func() { telemetry.Shutdown(context.Background()) })

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	pool, err := llm.NewPool(config.LLMPool{
		Default: "mock",
		Backends: []config.LLMConfig{{
			Name:      "mock",
			BaseURL:   srv.URL,
			APIKey:    "test",
			Model:     "test-model",
			MaxTokens: 64,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	reg, err := skill.NewRegistry(nil, config.CoreConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	reg.RegisterBuiltin("echo", "echo", echoExec{}, json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`))
	reg.RegisterBuiltin("code_exec", "code", echoExec{}, json.RawMessage(`{"type":"object","properties":{}}`))
	view := skill.NewView(reg, eagerSkills, slog.New(slog.NewTextHandler(io.Discard, nil)))

	b := bus.NewLocal(8)
	replyCh := b.Subscribe(bus.ReplyKey("http"))
	mem := memory.NewRAM()
	loop := New(config.AgentConfig{
		Name:         "tester",
		LLM:          "mock",
		MaxTurns:     2,
		SystemPrompt: "System",
	}, b, mem, 7, view, pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return loop, mem, replyCh
}

func TestNew_UsesConfiguredMemoryWindow(t *testing.T) {
	loop := New(config.AgentConfig{Name: "a"}, bus.NewLocal(1), memory.NewRAM(), 7, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if loop.memWindow != 7 {
		t.Fatalf("expected memory window 7, got %d", loop.memWindow)
	}
}

func TestNew_DefaultsMemoryWindow(t *testing.T) {
	loop := New(config.AgentConfig{Name: "a"}, bus.NewLocal(1), memory.NewRAM(), 0, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if loop.memWindow != 100 {
		t.Fatalf("expected default memory window 100, got %d", loop.memWindow)
	}
}

func TestRunOnce_TextReply(t *testing.T) {
	loop, mem, replyCh := newTestLoop(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
			"usage":   map[string]any{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5},
		})
	}, "echo")

	loop.RunOnce(context.Background(), &bus.Envelope{
		ID:             "req-1",
		ClientID:       "c1",
		ThreadID:       "t1",
		Role:           bus.RoleUser,
		Text:           "hello",
		SourceProtocol: "http",
		Meta:           map[string]string{},
		CreatedAt:      time.Now().UTC(),
	})

	select {
	case reply := <-replyCh:
		if reply.Text != "ok" || reply.Meta["tokens_total"] != "5" {
			t.Fatalf("unexpected reply: %+v", reply)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting reply")
	}

	msgs, err := mem.Get("c1", "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("unexpected memory history: %+v", msgs)
	}
}

func TestRunOnce_ToolCallAndFinalReply(t *testing.T) {
	var calls atomic.Int32
	loop, mem, replyCh := newTestLoop(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role":    "assistant",
						"content": nil,
						"tool_calls": []map[string]any{{
							"id":   "tool-1",
							"type": "function",
							"function": map[string]any{
								"name":      "echo",
								"arguments": `{"x":"1"}`,
							},
						}},
					},
				}},
				"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "done"}}},
			"usage":   map[string]any{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5},
		})
	})

	loop.RunOnce(context.Background(), &bus.Envelope{
		ID:             "req-2",
		ClientID:       "c2",
		ThreadID:       "t2",
		Role:           bus.RoleUser,
		Text:           "use tool",
		SourceProtocol: "http",
		Meta:           map[string]string{},
		CreatedAt:      time.Now().UTC(),
	})

	select {
	case reply := <-replyCh:
		if reply.Text != "done" {
			t.Fatalf("unexpected reply: %+v", reply)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting tool reply")
	}

	msgs, err := mem.Get("c2", "t2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 || msgs[2].Role != "tool" || !strings.Contains(msgs[2].Content, "echo:") {
		t.Fatalf("unexpected tool history: %+v", msgs)
	}
}

func TestRunOnce_LLMErrorAndMaxTurns(t *testing.T) {
	t.Run("llm_error", func(t *testing.T) {
		loop, _, replyCh := newTestLoop(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "timeout", http.StatusGatewayTimeout)
		})
		loop.RunOnce(context.Background(), &bus.Envelope{
			ID:             "req-3",
			ClientID:       "c3",
			ThreadID:       "t3",
			Role:           bus.RoleUser,
			Text:           "fail",
			SourceProtocol: "http",
			Meta:           map[string]string{},
			CreatedAt:      time.Now().UTC(),
		})
		select {
		case reply := <-replyCh:
			if !strings.Contains(strings.ToLower(reply.Text), "temporarily unavailable") {
				t.Fatalf("unexpected error reply: %s", reply.Text)
			}
		case <-time.After(time.Second):
			t.Fatal("timeout waiting error reply")
		}
	})

	t.Run("max_turns", func(t *testing.T) {
		loop, _, replyCh := newTestLoop(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role":    "assistant",
						"content": nil,
						"tool_calls": []map[string]any{{
							"id":   "tool-1",
							"type": "function",
							"function": map[string]any{
								"name":      "echo",
								"arguments": `{"x":"1"}`,
							},
						}},
					},
				}},
				"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
		})
		loop.cfg.MaxTurns = 1
		loop.RunOnce(context.Background(), &bus.Envelope{
			ID:             "req-4",
			ClientID:       "c4",
			ThreadID:       "t4",
			Role:           bus.RoleUser,
			Text:           "loop",
			SourceProtocol: "http",
			Meta:           map[string]string{},
			CreatedAt:      time.Now().UTC(),
		})
		select {
		case reply := <-replyCh:
			if !strings.Contains(reply.Text, "maximum reasoning steps") {
				t.Fatalf("unexpected max-turn reply: %s", reply.Text)
			}
		case <-time.After(time.Second):
			t.Fatal("timeout waiting max-turn reply")
		}
	})
}

func TestDeliverRunHelpersAndPromptBranches(t *testing.T) {
	loop, _, _ := newTestLoop(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	})
	loop.Deliver(nil)
	for i := 0; i < cap(loop.inbox)+2; i++ {
		loop.Deliver(&bus.Envelope{ClientID: "c", ThreadID: "t", SourceProtocol: "http"})
	}
	if loop.clientID != "c" || loop.threadID != "t" {
		t.Fatalf("expected routing info capture, got client=%s thread=%s", loop.clientID, loop.threadID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop.Run(ctx)
	loop.RunOnce(context.Background(), nil)

	if !strings.Contains(loop.buildSystemPrompt(), "System") {
		t.Fatal("expected configured prompt")
	}
	loop.skills.Activate("code_exec")
	if !strings.Contains(loop.buildSystemPrompt(), "reproducible execution") {
		t.Fatal("expected code_exec guidance in prompt")
	}
	defaultLoop := New(config.AgentConfig{}, bus.NewLocal(1), memory.NewRAM(), 1, loop.skills, loop.llms, loop.log)
	if !strings.Contains(defaultLoop.buildSystemPrompt(), "helpful, concise assistant") {
		t.Fatal("expected default prompt")
	}
}

func TestUserFacingLLMError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "LLM provider error"},
		{errors.New("tool_use_failed"), "invalid tool call"},
		{errors.New("llm api 400"), "simpler prompt"},
		{errors.New("timeout"), "temporarily unavailable"},
	}
	for _, tc := range cases {
		if got := strings.ToLower(userFacingLLMError(tc.err)); !strings.Contains(got, strings.ToLower(tc.want)) {
			t.Fatalf("unexpected user error for %v: %s", tc.err, got)
		}
	}
}

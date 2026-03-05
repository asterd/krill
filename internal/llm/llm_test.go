package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/telemetry"
)

func newTestBackend(t *testing.T, handler http.HandlerFunc) *Backend {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Backend{
		cfg: config.LLMConfig{
			Name:      "test",
			BaseURL:   srv.URL,
			APIKey:    "k",
			Model:     "m",
			MaxTokens: 32,
		},
		client: srv.Client(),
	}
}

func TestComplete_Success(t *testing.T) {
	telemetry.Configure(telemetry.Config{Profile: "off", Exporter: "none"}, nil)
	defer telemetry.Shutdown(context.Background())

	backend := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "ok"}},
			},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	})
	resp, err := backend.Complete(context.Background(), Request{
		ModelName:    "test",
		SystemPrompt: "sys",
		Messages:     []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok" || resp.Message.Role != "assistant" || resp.Usage.TotalTokens != 2 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestComplete_ErrorBranches(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "http_500",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
		},
		{
			name: "invalid_json",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("{"))
			},
		},
		{
			name: "empty_choices",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			backend := newTestBackend(t, tc.handler)
			if _, err := backend.Complete(context.Background(), Request{ModelName: "test"}); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestComplete_RequestShapeWithToolsAndNullContent(t *testing.T) {
	backend := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["max_tokens"] == nil {
			t.Fatal("expected max_tokens in request")
		}
		msgs, _ := payload["messages"].([]any)
		if len(msgs) < 2 {
			t.Fatalf("expected multiple messages, got %d", len(msgs))
		}
		if _, ok := payload["tools"]; !ok {
			t.Fatal("expected tools in request payload")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":       "assistant",
						"content":    nil,
						"tool_calls": []map[string]any{{"id": "1", "type": "function", "function": map[string]any{"name": "tool", "arguments": "{}"}}},
					},
				},
			},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
		})
	})

	tool := ToolDef{Type: "function"}
	tool.Function.Name = "tool"
	tool.Function.Description = "desc"
	tool.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
	resp, err := backend.Complete(context.Background(), Request{
		ModelName: "test",
		Messages: []Message{
			{
				Role: "assistant",
				ToolCalls: []ToolCall{{ID: "1", Type: "function", Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: "tool", Arguments: "{}"}}},
			},
			{Role: "tool", Content: "done", ToolCallID: "1"},
		},
		Tools: []ToolDef{tool},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "" || len(resp.ToolCalls) != 1 {
		t.Fatalf("unexpected response for null content/tool call: %+v", resp)
	}
}

func TestPoolGetFallbackAndErrors(t *testing.T) {
	pool, err := NewPool(config.LLMPool{
		Default: "a",
		Backends: []config.LLMConfig{
			{Name: "a", BaseURL: "http://x", APIKey: "k", Model: "m"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Get("missing"); err != nil {
		t.Fatal("expected fallback to default")
	}
	if _, err := NewPool(config.LLMPool{}); err == nil {
		t.Fatal("expected no backend error")
	}

	poolNoDefault, err := NewPool(config.LLMPool{
		Default: "missing",
		Backends: []config.LLMConfig{
			{Name: "only", BaseURL: "http://x", APIKey: "k", Model: "m"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := poolNoDefault.Get("ghost"); err == nil {
		t.Fatal("expected Get error without requested backend and missing default")
	}
}

func TestComplete_RetryWithoutToolsOnToolUseFailed(t *testing.T) {
	callCount := 0
	backend := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if callCount == 1 {
			if _, ok := payload["tools"]; !ok {
				t.Fatal("first call should include tools")
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"tool_use_failed","message":"Failed to call a function","failed_generation":"<function=tool>"}}`))
			return
		}
		if _, ok := payload["tools"]; ok {
			t.Fatal("retry call must remove tools")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "fallback-ok"}},
			},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	})

	tool := ToolDef{Type: "function"}
	tool.Function.Name = "tool"
	tool.Function.Description = "desc"
	tool.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)

	resp, err := backend.Complete(context.Background(), Request{
		ModelName:    "test",
		SystemPrompt: "sys",
		Messages:     []Message{{Role: "user", Content: "hello"}},
		Tools:        []ToolDef{tool},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "fallback-ok" {
		t.Fatalf("unexpected retry response: %+v", resp)
	}
	if callCount != 2 {
		t.Fatalf("expected retry flow with 2 calls, got %d", callCount)
	}
}

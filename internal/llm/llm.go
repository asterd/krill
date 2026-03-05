// Package llm — thin OpenAI-compatible HTTP client with multi-backend pooling.
// No SDK dependency: raw HTTP + json. Timeout per request. Failover on 5xx.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/telemetry"
)

// ─── Types ────────────────────────────────────────────────────────────────────

// Message is one turn in a conversation.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is an LLM-requested function invocation.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // always "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON
	} `json:"function"`
}

// ToolDef is the function definition sent to the LLM.
type ToolDef struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"` // JSON Schema
	} `json:"function"`
}

// Request to the LLM.
type Request struct {
	ModelName    string
	SystemPrompt string
	Messages     []Message
	Tools        []ToolDef // empty = no tool calling
	MaxTokens    int
}

// Response from the LLM.
type Response struct {
	Content   string     // text reply (when no tool calls)
	ToolCalls []ToolCall // function calls requested
	Message   Message    // raw message for appending to history
	Usage     Usage      // token usage if provided by backend
}

// Usage is token accounting returned by compatible LLM APIs.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ─── Backend ──────────────────────────────────────────────────────────────────

type Backend struct {
	cfg    config.LLMConfig
	client *http.Client
}

// Pool holds multiple backends; Get() does name lookup with default fallback.
type Pool struct {
	backends map[string]*Backend
	def      string
}

func NewPool(cfg config.LLMPool) (*Pool, error) {
	p := &Pool{backends: make(map[string]*Backend), def: cfg.Default}
	for _, bc := range cfg.Backends {
		p.backends[bc.Name] = &Backend{
			cfg:    bc,
			client: &http.Client{Timeout: 120 * time.Second},
		}
	}
	if len(p.backends) == 0 {
		return nil, fmt.Errorf("llm: no backends configured")
	}
	return p, nil
}

func (p *Pool) Get(name string) (*Backend, error) {
	if b, ok := p.backends[name]; ok {
		return b, nil
	}
	if b, ok := p.backends[p.def]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("llm: no backend %q and no default", name)
}

// Complete sends a chat completion request. Returns parsed Response.
func (b *Backend) Complete(ctx context.Context, req Request) (*Response, error) {
	traceID, parentSpanID, requestID := telemetry.TraceFromContext(ctx)
	span := telemetry.StartSpan(nil, traceID, parentSpanID, "llm.call",
		"request_id", requestID,
		"model_name", req.ModelName,
		"provider_model", b.cfg.Model,
	)
	var endErr error
	defer func() {
		span.End(endErr, "request_id", requestID)
	}()

	resp, statusCode, body, err := b.completeOnce(ctx, req)
	if err == nil {
		return resp, nil
	}
	if shouldRetryWithoutTools(statusCode, body, req) {
		retryReq := req
		retryReq.Tools = nil
		retryReq.SystemPrompt = strings.TrimSpace(req.SystemPrompt + "\n\nTool calling is unavailable for this request. Respond directly without calling tools.")
		resp, _, _, retryErr := b.completeOnce(ctx, retryReq)
		if retryErr == nil {
			return resp, nil
		}
		endErr = retryErr
		return nil, retryErr
	}
	endErr = err
	return nil, err
}

func (b *Backend) completeOnce(ctx context.Context, req Request) (*Response, int, []byte, error) {
	var msgs []map[string]interface{}

	// System prompt first
	if req.SystemPrompt != "" {
		msgs = append(msgs, map[string]interface{}{"role": "system", "content": req.SystemPrompt})
	}

	// Conversation history
	for _, m := range req.Messages {
		entry := map[string]interface{}{"role": m.Role, "content": m.Content}
		if len(m.ToolCalls) > 0 {
			entry["tool_calls"] = m.ToolCalls
			entry["content"] = nil // OpenAI spec: content null when tool_calls present
		}
		if m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		msgs = append(msgs, entry)
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = b.cfg.MaxTokens
	}
	if maxTokens == 0 {
		maxTokens = 4096
	}

	body := map[string]interface{}{
		"model":      b.cfg.Model,
		"messages":   msgs,
		"max_tokens": maxTokens,
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}

	payload, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(b.cfg.BaseURL, "/")+"/v1/chat/completions",
		bytes.NewReader(payload))
	if err != nil {
		return nil, 0, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+b.cfg.APIKey)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("llm http: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, data, fmt.Errorf("llm api %d: %s", resp.StatusCode, data)
	}

	var apiResp struct {
		Choices []struct {
			Message struct {
				Role      string     `json:"role"`
				Content   *string    `json:"content"` // pointer: can be null
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.Unmarshal(data, &apiResp); err != nil {
		return nil, resp.StatusCode, data, fmt.Errorf("llm parse: %w", err)
	}
	if len(apiResp.Choices) == 0 {
		return nil, resp.StatusCode, data, fmt.Errorf("llm: empty choices")
	}

	c := apiResp.Choices[0].Message
	content := ""
	if c.Content != nil {
		content = *c.Content
	}

	// Build raw message for history (preserve tool_calls for multi-turn)
	rawMsg := Message{Role: c.Role, Content: content, ToolCalls: c.ToolCalls}

	return &Response{
		Content:   content,
		ToolCalls: c.ToolCalls,
		Message:   rawMsg,
		Usage:     apiResp.Usage,
	}, resp.StatusCode, data, nil
}

func shouldRetryWithoutTools(statusCode int, body []byte, req Request) bool {
	if statusCode != http.StatusBadRequest || len(req.Tools) == 0 {
		return false
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "tool_use_failed") || strings.Contains(text, "failed to call a function")
}

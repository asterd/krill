// Package agent — per-client ReAct agent loop with progressive skill loading.
//
// ReAct cycle:
//  1. User message arrives via Deliver()
//  2. Build LLM request: system prompt + history (windowed) + ACTIVE tool defs
//  3. Call LLM
//     4a. Text response → publish reply → wait for next message
//     4b. Tool call → execute skill → if lazy-loaded, no special handling needed
//     (the tool def will be included next turn automatically via View)
//     → append tool result → goto 3
//  5. Max turns guard → apologize and exit ReAct
//  6. Idle timeout → goroutine exits
//
// Reply routing:
//
//	Replies are published to bus key "__reply__:<sourceProtocol>".
//	The originating protocol plugin subscribes to this key and filters by ClientID.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/skill"
	"github.com/krill/krill/internal/telemetry"
)

const idleTimeout = 30 * time.Minute

// Loop is one stateful agent execution context per client session.
type Loop struct {
	cfg    config.AgentConfig
	bus    bus.Bus
	mem    memory.Store
	skills *skill.View // per-loop view with progressive loading
	llms   *llm.Pool
	log    *slog.Logger

	inbox     chan *bus.Envelope // inbound user messages
	clientID  string
	threadID  string
	protocol  string // originating protocol for reply routing
	traceID   string
	requestID string
}

type tokenTotals struct {
	prompt     int
	completion int
	total      int
}

// New creates a Loop. Call Run() in a goroutine to start it.
func New(
	cfg config.AgentConfig,
	b bus.Bus,
	mem memory.Store,
	skillView *skill.View,
	llms *llm.Pool,
	log *slog.Logger,
) *Loop {
	return &Loop{
		cfg:    cfg,
		bus:    b,
		mem:    mem,
		skills: skillView,
		llms:   llms,
		log:    log,
		inbox:  make(chan *bus.Envelope, 32),
	}
}

// Deliver enqueues an inbound user envelope.
func (l *Loop) Deliver(env *bus.Envelope) {
	// Capture routing info from first message
	if l.clientID == "" {
		l.clientID = env.ClientID
		l.threadID = env.ThreadID
		l.protocol = env.SourceProtocol
	}
	select {
	case l.inbox <- env:
	default:
		l.log.Warn("inbox full, dropping message", "client", l.clientID)
	}
}

// Run blocks until context is cancelled or idle timeout fires.
func (l *Loop) Run(ctx context.Context) {
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-idle.C:
			l.log.Info("agent loop idle timeout", "client", l.clientID)
			return
		case env := <-l.inbox:
			idle.Reset(idleTimeout)
			// Update routing info (protocol may change across messages)
			l.protocol = env.SourceProtocol
			if env.ThreadID != "" {
				l.threadID = env.ThreadID
			}
			l.react(ctx, env)
		}
	}
}

// react runs one full ReAct cycle for a user message.
func (l *Loop) react(ctx context.Context, userEnv *bus.Envelope) {
	traceID := userEnv.Meta["trace_id"]
	if traceID == "" {
		traceID = telemetry.NewTraceID()
	}
	requestID := userEnv.Meta["request_id"]
	if requestID == "" {
		requestID = userEnv.ID
	}
	l.traceID = traceID
	l.requestID = requestID

	reqSpan := telemetry.StartSpan(l.log, traceID, userEnv.Meta["ingress_span"], "agent.react",
		"request_id", requestID,
		"client", l.clientID,
		"thread", l.threadID,
		"agent", l.cfg.Name,
	)
	defer reqSpan.End(nil,
		"request_id", requestID,
		"client", l.clientID,
		"thread", l.threadID,
	)

	// Store user message in memory
	l.mem.Append(l.clientID, l.threadID, llm.Message{
		Role:    "user",
		Content: userEnv.Text,
	})

	maxTurns := l.cfg.MaxTurns
	if maxTurns == 0 {
		maxTurns = 20
	}

	backend, err := l.llms.Get(l.cfg.LLM)
	if err != nil {
		l.reply(ctx, fmt.Sprintf("Configuration error: %v", err), tokenTotals{})
		return
	}

	tokens := tokenTotals{}
	for turn := 0; turn < maxTurns; turn++ {
		turnSpan := telemetry.StartSpan(l.log, traceID, reqSpan.SpanID(), "agent.turn",
			"request_id", requestID,
			"turn", turn,
			"llm", l.cfg.LLM,
		)
		// Build request with WINDOWED history and ACTIVE tool defs
		history := l.mem.Get(l.clientID, l.threadID, 100)
		req := llm.Request{
			ModelName:    l.cfg.LLM,
			SystemPrompt: l.buildSystemPrompt(),
			Messages:     history,
			Tools:        l.skills.ActiveToolDefs(), // ← progressive: only active skills
		}

		callStarted := time.Now()
		resp, err := backend.Complete(ctx, req)
		if err != nil {
			l.log.Error("llm error", "err", err, "turn", turn)
			turnSpan.End(err,
				"request_id", requestID,
				"turn", turn,
			)
			l.reply(ctx, userFacingLLMError(err), tokens)
			return
		}
		tokens.prompt += resp.Usage.PromptTokens
		tokens.completion += resp.Usage.CompletionTokens
		tokens.total += resp.Usage.TotalTokens
		l.log.Info("llm call completed",
			"trace_id", traceID,
			"request_id", requestID,
			"turn", turn,
			"llm", l.cfg.LLM,
			"duration_ms", time.Since(callStarted).Milliseconds(),
			"usage_prompt_tokens", resp.Usage.PromptTokens,
			"usage_completion_tokens", resp.Usage.CompletionTokens,
			"usage_total_tokens", resp.Usage.TotalTokens,
			"cumulative_prompt_tokens", tokens.prompt,
			"cumulative_completion_tokens", tokens.completion,
			"cumulative_total_tokens", tokens.total,
			"tool_calls", len(resp.ToolCalls),
		)

		// Store assistant message (with tool_calls if any)
		l.mem.Append(l.clientID, l.threadID, resp.Message)

		if len(resp.ToolCalls) == 0 {
			// Final text response — send to user
			turnSpan.End(nil,
				"request_id", requestID,
				"turn", turn,
				"finish_reason", "assistant_text",
				"cumulative_total_tokens", tokens.total,
			)
			l.reply(ctx, resp.Content, tokens)
			return
		}

		// Process tool calls
		for _, tc := range resp.ToolCalls {
			toolSpan := telemetry.StartSpan(l.log, traceID, turnSpan.SpanID(), "agent.tool_call",
				"request_id", requestID,
				"turn", turn,
				"skill", tc.Function.Name,
				"client", l.clientID,
			)
			l.log.Info("skill call", "skill", tc.Function.Name, "client", l.clientID, "turn", turn,
				"trace_id", traceID, "request_id", requestID)

			toolStarted := time.Now()
			toolCtx := telemetry.WithTrace(ctx, traceID, toolSpan.SpanID(), requestID)
			result, lazyResult, execErr := l.skills.Execute(toolCtx, tc.Function.Name, tc.Function.Arguments)

			if execErr != nil {
				l.log.Warn("skill error", "skill", tc.Function.Name, "err", execErr)
				result = fmt.Sprintf("error: %v", execErr)
				toolSpan.End(execErr,
					"duration_ms", time.Since(toolStarted).Milliseconds(),
					"result_bytes", len(result),
				)
			} else if lazyResult == skill.LazyLoaded {
				// Skill was lazy-loaded — log it; it will appear in future turns
				l.log.Info("skill lazy-loaded", "skill", tc.Function.Name,
					"active_count", l.skills.ActiveCount())
				toolSpan.End(nil,
					"lazy_loaded", true,
					"duration_ms", time.Since(toolStarted).Milliseconds(),
					"result_bytes", len(result),
				)
			} else {
				toolSpan.End(nil,
					"lazy_loaded", false,
					"duration_ms", time.Since(toolStarted).Milliseconds(),
					"result_bytes", len(result),
				)
			}

			// Append tool result to memory
			l.mem.Append(l.clientID, l.threadID, llm.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
		turnSpan.End(nil,
			"request_id", requestID,
			"turn", turn,
			"finish_reason", "tool_calls",
			"cumulative_total_tokens", tokens.total,
		)
		// Continue ReAct loop — next iteration will include updated skill set
	}

	// Max turns reached
	l.reply(ctx, "I've reached the maximum reasoning steps. Please simplify your request.", tokens)
}

// reply publishes an assistant reply to the correct protocol reply channel.
func (l *Loop) reply(ctx context.Context, text string, tokens tokenTotals) {
	meta := map[string]string{
		"agent":             l.cfg.Name,
		"trace_id":          l.traceID,
		"request_id":        l.requestID,
		"tokens_prompt":     strconv.Itoa(tokens.prompt),
		"tokens_completion": strconv.Itoa(tokens.completion),
		"tokens_total":      strconv.Itoa(tokens.total),
	}
	env := &bus.Envelope{
		ID:             uuid.NewString(),
		ClientID:       l.clientID,
		ThreadID:       l.threadID,
		Role:           bus.RoleAssistant,
		Text:           text,
		SourceProtocol: l.protocol,
		Meta:           meta,
		CreatedAt:      time.Now(),
	}
	// Publish to protocol-specific reply channel
	replyKey := bus.ReplyKey(l.protocol)
	if err := l.bus.Publish(ctx, replyKey, env); err != nil {
		l.log.Warn("reply publish failed", "err", err, "key", replyKey)
		return
	}
	l.log.Info("reply published",
		"trace_id", l.traceID,
		"request_id", l.requestID,
		"client", l.clientID,
		"agent", l.cfg.Name,
		"reply_key", replyKey,
		"reply_bytes", len(text),
		"tokens_total", tokens.total,
	)
}

// buildSystemPrompt assembles the system prompt.
// Includes active skill count as a hint to the LLM.
func (l *Loop) buildSystemPrompt() string {
	base := l.cfg.SystemPrompt
	if base == "" {
		base = "You are a helpful, concise assistant."
	}
	// Inform LLM about available tools count to prevent hallucination
	if l.skills.ActiveCount() > 0 {
		base += fmt.Sprintf("\n\nYou have %d tools available. Use them when appropriate.", l.skills.ActiveCount())
		base += "\nWhen calling a tool, function.arguments must be strict JSON object text."
		base += "\nDo not emit XML-like tool tags such as <function=...>."
	}
	if l.skills.IsActive("code_exec") {
		base += "\nWhen a request requires exact computation, code generation validation, parsing, or reproducible execution, use code_exec and base your final answer on its result."
	}
	return base
}

func userFacingLLMError(err error) string {
	msg := "I encountered an LLM provider error. Please try again."
	if err == nil {
		return msg
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "tool_use_failed") || strings.Contains(low, "failed to call a function") {
		return "The model returned an invalid tool call format. Please retry; I will continue without relying on that tool format."
	}
	if strings.Contains(low, "llm api 4") {
		return "The LLM provider rejected this request. Please retry with a simpler prompt."
	}
	if strings.Contains(low, "llm api 5") || strings.Contains(low, "timeout") {
		return "The LLM provider is temporarily unavailable. Please retry shortly."
	}
	return msg
}

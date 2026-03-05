// Package orchestrator — multi-client agent-loop lifecycle manager.
//
// Routing strategies (select which AgentConfig to assign to a client):
//
//	"first"    — always use first agent in config (default)
//	"protocol" — match agent.MatchProtocol to envelope.SourceProtocol
//	"round_robin" — distribute clients across agents evenly
//
// Each client gets ONE Loop goroutine. The orchestrator is the only component
// that creates and destroys Loop goroutines. It enforces max_clients via semaphore.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/agent"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/session"
	"github.com/krill/krill/internal/skill"
	"github.com/krill/krill/internal/telemetry"
)

// Orch manages per-client agent loops.
type Orch struct {
	cfg      *config.Root
	b        bus.Bus
	skillReg *skill.Registry
	mem      memory.Store
	llms     *llm.Pool
	log      *slog.Logger
	sessions *session.Service

	mu      sync.Mutex
	loops   map[string]*agent.Loop // clientID → loop
	sem     chan struct{}          // capacity = max_clients
	rrIndex atomic.Int64           // round-robin cursor

	wfMu     sync.Mutex
	wfStates map[string]WorkflowState // workflow request state by envelope id
}

// New constructs an orchestrator with the configured runtime dependencies.
func New(
	cfg *config.Root,
	b bus.Bus,
	skillReg *skill.Registry,
	mem memory.Store,
	llms *llm.Pool,
	log *slog.Logger,
	sessions *session.Service,
) (*Orch, error) {
	maxClients := cfg.Core.MaxClients
	if maxClients == 0 {
		maxClients = 1000
	}
	return &Orch{
		cfg:      cfg,
		b:        b,
		skillReg: skillReg,
		mem:      mem,
		llms:     llms,
		log:      log,
		sessions: sessions,
		loops:    make(map[string]*agent.Loop),
		sem:      make(chan struct{}, maxClients),
		wfStates: make(map[string]WorkflowState),
	}, nil
}

// Start launches the inbound dispatch loop.
func (o *Orch) Start(ctx context.Context) error {
	inCh := o.b.Subscribe(bus.InboundKey)
	go o.dispatch(ctx, inCh)
	o.log.Info("orchestrator started", "max_clients", cap(o.sem))
	return nil
}

// dispatch routes inbound user envelopes to per-client agent loops.
func (o *Orch) dispatch(ctx context.Context, in <-chan *bus.Envelope) {
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-in:
			if !ok {
				return
			}
			consumeSpan := telemetry.StartSpan(nil, env.Meta["trace_id"], env.Meta["ingress_span"], "bus.consume",
				"client", env.ClientID,
				"protocol", env.SourceProtocol,
			)
			if env.Role != bus.RoleUser {
				consumeSpan.End(nil, "skipped_role", string(env.Role))
				continue // system/tool messages are not user-initiated
			}
			if env.Meta == nil {
				env.Meta = map[string]string{}
			}
			env.Meta["consume_span"] = consumeSpan.SpanID()
			consumeSpan.End(nil)
			o.route(ctx, env)
		}
	}
}

// route ensures a running loop for the client and delivers the envelope.
func (o *Orch) route(ctx context.Context, env *bus.Envelope) {
	if wf, ok := o.workflowFor(env); ok && isCooperativeWorkflow(wf) {
		o.routeCooperative(ctx, env, wf)
		return
	}

	routeSpan := telemetry.StartSpan(nil, env.Meta["trace_id"], env.Meta["consume_span"], "orchestrator.route",
		"client", env.ClientID,
	)
	defer routeSpan.End(nil, "client", env.ClientID)

	o.mu.Lock()
	loop, exists := o.loops[env.ClientID]
	if !exists {
		// Semaphore: non-blocking check
		select {
		case o.sem <- struct{}{}:
		default:
			o.mu.Unlock()
			o.log.Warn("max_clients reached, dropping",
				"client", env.ClientID, "active", len(o.loops))
			return
		}

		o.restoreSessionContext(env)
		agentCfg := o.selectAgent(env, routeSpan.SpanID())
		skillView := o.buildSkillView(agentCfg)
		loop = agent.New(agentCfg, o.b, o.mem, o.cfg.Core.MemoryWindow, skillView, o.llms, o.log)

		loopCtx, cancel := context.WithCancel(ctx)
		o.loops[env.ClientID] = loop
		loop.Deliver(env)

		go func(clientID string) {
			defer func() {
				cancel()
				<-o.sem
				o.mu.Lock()
				delete(o.loops, clientID)
				o.mu.Unlock()
				telemetry.SetGauge(telemetry.MetricActiveLoops, int64(o.ActiveCount()), nil)
				o.log.Info("agent loop exited", "client", clientID)
			}()
			loop.Run(loopCtx)
		}(env.ClientID)

		o.log.Info("agent loop started",
			"client", env.ClientID,
			"agent", agentCfg.Name,
			"protocol", env.SourceProtocol,
			"active_loops", len(o.loops))
		telemetry.SetGauge(telemetry.MetricActiveLoops, int64(len(o.loops)), nil)
		o.mu.Unlock()
		return
	}
	o.mu.Unlock()

	loop.Deliver(env)
}

// selectAgent picks the best AgentConfig for the incoming envelope.
// Priority: protocol match > round-robin > first.
func (o *Orch) selectAgent(env *bus.Envelope, parentSpanID string) config.AgentConfig {
	span := telemetry.StartSpan(nil, env.Meta["trace_id"], parentSpanID, "orchestrator.select_agent",
		"protocol", env.SourceProtocol,
	)
	defer span.End(nil)
	agents := o.cfg.Agents
	if len(agents) == 0 {
		return defaultAgent(o.cfg.LLM.Default)
	}

	if env != nil && env.Meta != nil {
		target := strings.TrimSpace(env.Meta["target_agent"])
		if target != "" {
			for _, a := range agents {
				if strings.TrimSpace(a.Name) == target {
					return a
				}
			}
		}
	}

	// 1. Exact protocol match
	for _, a := range agents {
		if a.MatchProtocol != "" && a.MatchProtocol == env.SourceProtocol {
			return a
		}
	}

	// 2. Round-robin across agents without a protocol restriction
	var candidates []config.AgentConfig
	for _, a := range agents {
		if a.MatchProtocol == "" {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) > 0 {
		idx := int(o.rrIndex.Add(1)-1) % len(candidates)
		return candidates[idx]
	}

	telemetry.IncCounter(telemetry.MetricAgentHandoffTotal, 0, nil)
	return agents[0]
}

func (o *Orch) restoreSessionContext(env *bus.Envelope) {
	if o.sessions == nil || env == nil {
		return
	}
	if history := o.mem.Get(env.ClientID, env.ThreadID, 1); len(history) > 0 {
		return
	}

	if _, ok, err := o.sessions.ResumeByThread(env.ClientID, env.ThreadID, session.Provenance{
		Actor:  "orchestrator",
		Source: "route.resume",
		Meta:   map[string]string{"client_id": env.ClientID, "thread_id": env.ThreadID},
	}); err != nil {
		o.log.Warn("session resume failed", "client", env.ClientID, "thread", env.ThreadID, "err", err)
	} else if !ok && shouldOpenPersistentSession(env) {
		_, err := o.sessions.Open(session.OpenRequest{
			Tenant:   env.Meta["tenant"],
			ClientID: env.ClientID,
			ThreadID: env.ThreadID,
			Mode:     session.Mode(defaultSessionMode(env.Meta["session_mode"])),
		}, session.Provenance{Actor: "orchestrator", Source: "route.open"})
		if err != nil {
			o.log.Warn("session open failed", "client", env.ClientID, "thread", env.ThreadID, "err", err)
		}
	}

	msgs, ok, err := o.sessions.RestoreMessagesByThread(env.ClientID, env.ThreadID)
	if err != nil || !ok {
		return
	}
	if hydrator, ok := o.mem.(session.Hydrator); ok {
		hydrator.Hydrate(env.ClientID, env.ThreadID, msgs)
		return
	}
	for _, msg := range msgs {
		o.mem.Append(env.ClientID, env.ThreadID, msg)
	}
}

func shouldOpenPersistentSession(env *bus.Envelope) bool {
	if env == nil || env.Meta == nil {
		return true
	}
	return defaultSessionMode(env.Meta["session_mode"]) == string(session.ModePersistent)
}

func defaultSessionMode(value string) string {
	mode := strings.ToLower(strings.TrimSpace(value))
	if mode == "" {
		return string(session.ModePersistent)
	}
	return mode
}

// buildSkillView creates a per-loop skill view with the agent's eager skills pre-activated.
func (o *Orch) buildSkillView(agentCfg config.AgentConfig) *skill.View {
	return skill.NewView(o.skillReg, agentCfg.EagerSkills, o.log)
}

// ActiveCount returns the number of running agent loops.
func (o *Orch) ActiveCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.loops)
}

// defaultAgent returns a minimal agent config when none is configured.
func defaultAgent(llmName string) config.AgentConfig {
	return config.AgentConfig{
		Name:         "default",
		LLM:          llmName,
		MaxTurns:     20,
		SystemPrompt: "You are a helpful assistant.",
		EagerSkills:  []string{"http_fetch", "json_parse"},
	}
}

// Ensure Orch satisfies fmt.Stringer for debugging
func (o *Orch) String() string {
	return fmt.Sprintf("Orch{active:%d, cap:%d}", o.ActiveCount(), cap(o.sem))
}

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
	"sync"
	"sync/atomic"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/agent"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/skill"
)

// Orch manages per-client agent loops.
type Orch struct {
	cfg      *config.Root
	b        bus.Bus
	skillReg *skill.Registry
	mem      memory.Store
	llms     *llm.Pool
	log      *slog.Logger

	mu      sync.Mutex
	loops   map[string]*agent.Loop // clientID → loop
	sem     chan struct{}          // capacity = max_clients
	rrIndex atomic.Int64           // round-robin cursor
}

func New(
	cfg *config.Root,
	b bus.Bus,
	skillReg *skill.Registry,
	mem memory.Store,
	llms *llm.Pool,
	log *slog.Logger,
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
		loops:    make(map[string]*agent.Loop),
		sem:      make(chan struct{}, maxClients),
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
			if env.Role != bus.RoleUser {
				continue // system/tool messages are not user-initiated
			}
			o.route(ctx, env)
		}
	}
}

// route ensures a running loop for the client and delivers the envelope.
func (o *Orch) route(ctx context.Context, env *bus.Envelope) {
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

		agentCfg := o.selectAgent(env)
		skillView := o.buildSkillView(agentCfg)
		loop = agent.New(agentCfg, o.b, o.mem, skillView, o.llms, o.log)

		loopCtx, cancel := context.WithCancel(ctx)
		o.loops[env.ClientID] = loop

		go func(clientID string) {
			defer func() {
				cancel()
				<-o.sem
				o.mu.Lock()
				delete(o.loops, clientID)
				o.mu.Unlock()
				o.log.Info("agent loop exited", "client", clientID)
			}()
			loop.Run(loopCtx)
		}(env.ClientID)

		o.log.Info("agent loop started",
			"client", env.ClientID,
			"agent", agentCfg.Name,
			"protocol", env.SourceProtocol,
			"active_loops", len(o.loops))
	}
	o.mu.Unlock()

	loop.Deliver(env)
}

// selectAgent picks the best AgentConfig for the incoming envelope.
// Priority: protocol match > round-robin > first.
func (o *Orch) selectAgent(env *bus.Envelope) config.AgentConfig {
	agents := o.cfg.Agents
	if len(agents) == 0 {
		return defaultAgent(o.cfg.LLM.Default)
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

	return agents[0]
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

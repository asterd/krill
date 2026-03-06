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

type Orch struct {
	cfg      *config.Root
	b        bus.Bus
	skillReg *skill.Registry
	mem      memory.Store
	llms     *llm.Pool
	log      *slog.Logger
	sessions *session.Service

	mu         sync.Mutex
	threads    map[string]*threadState
	queueDepth int
	sem        chan struct{}
	work       chan string
	rrIndex    atomic.Int64

	wfMu     sync.Mutex
	wfStates map[string]WorkflowState
}

type threadState struct {
	agentCfg  config.AgentConfig
	skillView *skill.View
	queue     []*bus.Envelope
	running   bool
}

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
	queueDepth := cfg.Core.BusBuffer
	if queueDepth <= 0 {
		queueDepth = 32
	}
	return &Orch{
		cfg:        cfg,
		b:          b,
		skillReg:   skillReg,
		mem:        mem,
		llms:       llms,
		log:        log,
		sessions:   sessions,
		threads:    make(map[string]*threadState),
		queueDepth: queueDepth,
		sem:        make(chan struct{}, maxClients),
		work:       make(chan string, maxClients),
		wfStates:   make(map[string]WorkflowState),
	}, nil
}

func (o *Orch) Start(ctx context.Context) error {
	inCh := o.b.Subscribe(bus.InboundKey)
	for i := 0; i < cap(o.sem); i++ {
		go o.worker(ctx)
	}
	go o.dispatch(ctx, inCh)
	o.log.Info("orchestrator started", "workers", cap(o.sem))
	return nil
}

func (o *Orch) dispatch(ctx context.Context, in <-chan *bus.Envelope) {
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-in:
			if !ok {
				return
			}
			if env == nil || env.Role != bus.RoleUser {
				continue
			}
			if env.Meta == nil {
				env.Meta = map[string]string{}
			}
			o.route(ctx, env)
		}
	}
}

func (o *Orch) route(ctx context.Context, env *bus.Envelope) {
	if wf, ok := o.workflowFor(env); ok && isCooperativeWorkflow(wf) {
		o.routeCooperative(ctx, env, wf)
		return
	}

	o.restoreSessionContext(env)
	key := threadKey(env.ClientID, env.ThreadID)

	o.mu.Lock()
	state, exists := o.threads[key]
	if !exists {
		select {
		case o.sem <- struct{}{}:
		default:
			o.mu.Unlock()
			o.log.Warn("max active threads reached", "client", env.ClientID, "thread", env.ThreadID)
			return
		}
		agentCfg := o.selectAgent(env, "")
		state = &threadState{
			agentCfg:  agentCfg,
			skillView: o.buildSkillView(agentCfg),
		}
		o.threads[key] = state
	}
	if len(state.queue) >= o.queueDepth {
		o.mu.Unlock()
		o.log.Warn("thread queue overflow", "client", env.ClientID, "thread", env.ThreadID, "depth", len(state.queue))
		return
	}
	state.queue = append(state.queue, env)
	if !state.running {
		state.running = true
		o.work <- key
	}
	active := len(o.threads)
	o.mu.Unlock()
	telemetry.SetGauge(telemetry.MetricActiveLoops, int64(active), nil)
}

func (o *Orch) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case key := <-o.work:
			o.runThread(ctx, key)
		}
	}
}

func (o *Orch) runThread(ctx context.Context, key string) {
	for {
		o.mu.Lock()
		state := o.threads[key]
		if state == nil || len(state.queue) == 0 {
			if state != nil {
				state.running = false
				delete(o.threads, key)
				<-o.sem
			}
			active := len(o.threads)
			o.mu.Unlock()
			telemetry.SetGauge(telemetry.MetricActiveLoops, int64(active), nil)
			return
		}
		env := state.queue[0]
		state.queue = state.queue[1:]
		agentCfg := state.agentCfg
		skillView := state.skillView
		o.mu.Unlock()

		loop := agent.New(agentCfg, o.b, o.mem, o.cfg.Core.MemoryWindow, skillView, o.llms, o.log)
		loop.RunOnce(ctx, env)
	}
}

func (o *Orch) selectAgent(env *bus.Envelope, parentSpanID string) config.AgentConfig {
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
	for _, a := range agents {
		if a.MatchProtocol != "" && a.MatchProtocol == env.SourceProtocol {
			return a
		}
	}
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
	history, err := o.mem.Get(env.ClientID, env.ThreadID, 1)
	if err == nil && len(history) > 0 {
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
	if err := o.mem.Restore(env.ClientID, env.ThreadID, memory.Snapshot{Messages: msgs}); err != nil {
		o.log.Warn("memory restore failed", "client", env.ClientID, "thread", env.ThreadID, "err", err)
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

func (o *Orch) buildSkillView(agentCfg config.AgentConfig) *skill.View {
	return skill.NewView(o.skillReg, agentCfg.EagerSkills, o.log)
}

func (o *Orch) ActiveCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.threads)
}

func defaultAgent(llmName string) config.AgentConfig {
	return config.AgentConfig{
		Name:         "default",
		LLM:          llmName,
		MaxTurns:     20,
		SystemPrompt: "You are a helpful assistant.",
		EagerSkills:  []string{"http_fetch", "json_parse"},
	}
}

func (o *Orch) String() string {
	return fmt.Sprintf("Orch{active:%d, cap:%d}", o.ActiveCount(), cap(o.sem))
}

func threadKey(clientID, threadID string) string {
	return clientID + ":" + threadID
}

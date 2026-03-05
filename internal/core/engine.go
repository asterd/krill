// Package core — Engine: the single wiring object.
// Imports flow strictly downward:
//
//	engine → orchestrator → agent → skill/llm/memory
//	engine → protocol plugins (via registry, no direct import)
//
// No package imports engine except cmd/krill/main.go.
package core

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/orchestrator"
	"github.com/krill/krill/internal/scheduler"
	"github.com/krill/krill/internal/session"
	"github.com/krill/krill/internal/skill"
	"github.com/krill/krill/plugins/skill/builtins"
)

// Engine is the top-level runtime object. One per process.
type Engine struct {
	cfg    *config.Root
	log    *slog.Logger
	bus    bus.Bus
	protos []Protocol
	orch   *orchestrator.Orch
	sched  *scheduler.Engine
	skills *skill.Registry
	mem    memory.Store
}

// New wires and validates the engine from config. Does not start I/O.
func New(cfg *config.Root, log *slog.Logger) (*Engine, error) {
	// 1. Bus
	bus.SetReplyPrefix(cfg.Core.ReplyBusPrefix)
	b := bus.NewLocal(cfg.Core.BusBuffer)

	// 2. LLM pool
	llmPool, err := llm.NewPool(cfg.LLM)
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}

	// 3. Skill registry — loads config-defined skills
	skillReg, err := skill.NewRegistry(cfg.Skills, cfg.Core, log)
	if err != nil {
		return nil, fmt.Errorf("skills: %w", err)
	}

	// 4. Register builtin skills (pure Go, no external process)
	builtins.Register(skillReg, cfg.Core)
	log.Info("skills loaded", "total", skillReg.Count())

	// 5. Memory store (sqlite default, ram/file optional)
	mem, err := memory.NewStore(cfg.Core.MemoryBackend, cfg.Core.MemoryPath)
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}

	var sessionSvc *session.Service
	if cfg.Sessions.Enabled {
		sessionSvc, err = session.NewService(cfg.Sessions)
		if err != nil {
			return nil, fmt.Errorf("sessions: %w", err)
		}
		mem = session.WrapMemoryStore(mem, sessionSvc)
	}

	// 6. Orchestrator
	orch, err := orchestrator.New(cfg, b, skillReg, mem, llmPool, log, sessionSvc)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}

	var sched *scheduler.Engine
	if cfg.Scheduler.Enabled && len(cfg.Scheduler.Schedules) > 0 {
		sched, err = scheduler.NewEngine(cfg.Scheduler, scheduler.BusExecutor(b), log)
		if err != nil {
			return nil, fmt.Errorf("scheduler: %w", err)
		}
	}

	// 7. Protocol plugins — looked up from registry (plugins self-register via init())
	var protos []Protocol
	for _, ref := range cfg.Protocols {
		if !ref.Enabled {
			log.Info("protocol disabled", "name", ref.Name)
			continue
		}
		pCfg := copyConfigMap(ref.Config)
		pCfg["_strict_v2_validation"] = cfg.Core.StrictEnvelopeV2Validation
		p, err := Global().BuildProtocol(ref.Name, pCfg)
		if err != nil {
			return nil, fmt.Errorf("protocol %q: %w", ref.Name, err)
		}
		protos = append(protos, p)
		log.Info("protocol registered", "name", ref.Name)
	}

	return &Engine{
		cfg:    cfg,
		log:    log,
		bus:    b,
		protos: protos,
		orch:   orch,
		sched:  sched,
		skills: skillReg,
		mem:    mem,
	}, nil
}

func copyConfigMap(in map[string]interface{}) map[string]interface{} {
	if len(in) == 0 {
		return map[string]interface{}{}
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Run starts all subsystems and blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	// Start orchestrator first
	if err := e.orch.Start(ctx); err != nil {
		return fmt.Errorf("orchestrator: %w", err)
	}
	if e.sched != nil {
		tick := time.Duration(e.cfg.Scheduler.TickMs) * time.Millisecond
		e.sched.Start(ctx, tick)
		e.log.Info("scheduler started", "schedules", len(e.cfg.Scheduler.Schedules), "tick_ms", e.cfg.Scheduler.TickMs)
	}

	// Start protocol plugins
	for _, p := range e.protos {
		if err := p.Start(ctx, e.bus, e.log); err != nil {
			return fmt.Errorf("protocol %q: %w", p.Name(), err)
		}
		e.log.Info("protocol started", "name", p.Name())
	}

	e.log.Info("Krill running",
		"protocols", len(e.protos),
		"skills", e.skills.Count(),
		"max_clients", e.cfg.Core.MaxClients,
	)

	<-ctx.Done()
	e.log.Info("shutdown initiated")

	stopCtx := context.Background()
	for _, p := range e.protos {
		if err := p.Stop(stopCtx); err != nil {
			e.log.Warn("protocol stop error", "name", p.Name(), "err", err)
		}
	}
	if closer, ok := e.mem.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			e.log.Warn("memory close error", "err", err)
		}
	}
	return nil
}

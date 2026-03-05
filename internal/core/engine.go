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

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/orchestrator"
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
	skills *skill.Registry
}

// New wires and validates the engine from config. Does not start I/O.
func New(cfg *config.Root, log *slog.Logger) (*Engine, error) {
	// 1. Bus
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

	// 5. Memory store (RAM; swap for persistent backend)
	mem := memory.NewRAM()

	// 6. Orchestrator
	orch, err := orchestrator.New(cfg, b, skillReg, mem, llmPool, log)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}

	// 7. Protocol plugins — looked up from registry (plugins self-register via init())
	var protos []Protocol
	for _, ref := range cfg.Protocols {
		if !ref.Enabled {
			log.Info("protocol disabled", "name", ref.Name)
			continue
		}
		p, err := Global().BuildProtocol(ref.Name, ref.Config)
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
		skills: skillReg,
	}, nil
}

// Run starts all subsystems and blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	// Start orchestrator first
	if err := e.orch.Start(ctx); err != nil {
		return fmt.Errorf("orchestrator: %w", err)
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
	return nil
}

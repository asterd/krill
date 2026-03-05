// Package skill — the Krill skill marketplace with progressive lazy loading.
//
// # Lazy loading pattern (inspired by DeerFlow)
//
// Problem: giving the LLM 50 tool definitions at once bloats the context,
// increases cost, and confuses smaller models.
//
// Solution: the skill registry has two tiers:
//
//	EAGER tier  — always present in the LLM request (small, always-useful tools)
//	LAZY tier   — not sent to LLM unless explicitly activated
//
// Lazy skill activation happens two ways:
//  1. Automatic: if the LLM calls a skill name that IS registered but NOT in
//     the active set, the registry auto-promotes it and retries the turn
//     (transparent to the agent loop).
//  2. Explicit: a skill can declare "trigger_skills" — when it runs, those
//     skills are promoted for the remainder of the conversation.
//
// This keeps the active tool set lean (<10 tools ideally) while making all
// registered skills reachable.
package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/sandbox"
	"github.com/krill/krill/internal/telemetry"
)

// Executor is the execution interface for builtin (Go-native) skills.
type Executor interface {
	Execute(ctx context.Context, argsJSON string) (string, error)
}

// loggerAware is an optional interface for executors that want structured logs.
type loggerAware interface {
	SetLogger(log *slog.Logger)
}

// Factory is a constructor for builtin skills.
type Factory func(cfg map[string]interface{}) (Executor, error)

// entry holds a skill's definition and its executor.
type entry struct {
	cfg     config.SkillConfig
	toolDef llm.ToolDef
	exec    Executor        // non-nil for builtins
	sb      sandbox.Sandbox // non-nil for wasm/exec skills
	tags    map[string]bool
}

// ─── Registry ─────────────────────────────────────────────────────────────────

// Registry is the global skill store. Thread-safe.
// Use NewView() to get a per-agent-loop view with its own active set.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*entry
	log     *slog.Logger
}

// NewRegistry constructs the skill registry and loads configured skills.
func NewRegistry(skills []config.SkillConfig, core config.CoreConfig, log *slog.Logger) (*Registry, error) {
	r := &Registry{
		entries: make(map[string]*entry),
		log:     log,
	}
	for _, sc := range skills {
		if err := r.add(sc, core); err != nil {
			return nil, fmt.Errorf("skill %q: %w", sc.Name, err)
		}
	}
	return r, nil
}

func (r *Registry) add(sc config.SkillConfig, core config.CoreConfig) error {
	e := &entry{
		cfg:  sc,
		tags: make(map[string]bool),
	}
	for _, t := range sc.Tags {
		e.tags[t] = true
	}

	// Build LLM tool definition
	params := json.RawMessage(`{"type":"object","properties":{}}`)
	if sc.InputSchema != "" {
		params = json.RawMessage(sc.InputSchema)
	}
	e.toolDef.Type = "function"
	e.toolDef.Function.Name = sc.Name
	e.toolDef.Function.Description = sc.Description
	e.toolDef.Function.Parameters = params

	switch sc.Runtime {
	case "builtin":
		// builtin executors are registered via RegisterBuiltin
	case "exec":
		sb, err := sandbox.NewProcess(sandbox.ProcessConfig{
			Path: sc.Path, TimeoutMs: core.SkillTimeoutMs, Env: sc.Env,
		})
		if err != nil {
			return err
		}
		e.sb = sb
	case "wasm":
		sb, err := sandbox.NewWASM(sandbox.WASMConfig{
			Path: sc.Path, Fuel: core.WasmFuel, TimeoutMs: core.SkillTimeoutMs, Env: sc.Env,
		})
		if err != nil {
			return err
		}
		e.sb = sb
	case "":
		// will be filled by RegisterBuiltin later
	default:
		return fmt.Errorf("unknown runtime %q", sc.Runtime)
	}

	r.mu.Lock()
	r.entries[sc.Name] = e
	r.mu.Unlock()
	return nil
}

// RegisterBuiltin adds or overwrites a skill with a native Go executor.
// Called from plugin init() or builtins.Register().
func (r *Registry) RegisterBuiltin(name, description string, ex Executor, schema json.RawMessage) {
	if schema == nil {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	params := schema
	td := llm.ToolDef{Type: "function"}
	td.Function.Name = name
	td.Function.Description = description
	td.Function.Parameters = params

	r.mu.Lock()
	tags := map[string]bool{}
	if existing, ok := r.entries[name]; ok {
		for tag := range existing.tags {
			tags[tag] = true
		}
	}
	r.entries[name] = &entry{
		cfg:     config.SkillConfig{Name: name, Description: description, Runtime: "builtin"},
		toolDef: td,
		exec:    ex,
		tags:    tags,
	}
	r.mu.Unlock()
	if la, ok := ex.(loggerAware); ok {
		la.SetLogger(r.log)
	}
}

// Exists returns whether a skill name is registered.
func (r *Registry) Exists(name string) bool {
	r.mu.RLock()
	_, ok := r.entries[name]
	r.mu.RUnlock()
	return ok
}

// Execute runs a skill. Returns result or error.
func (r *Registry) Execute(ctx context.Context, name, argsJSON string) (string, error) {
	traceID, parentSpanID, requestID := telemetry.TraceFromContext(ctx)
	span := telemetry.StartSpan(nil, traceID, parentSpanID, "skill.execute",
		"request_id", requestID,
		"skill", name,
	)
	var endErr error
	defer func() {
		span.End(endErr, "request_id", requestID, "skill", name)
	}()

	r.mu.RLock()
	e, ok := r.entries[name]
	r.mu.RUnlock()
	if !ok {
		endErr = fmt.Errorf("not found")
		return "", fmt.Errorf("skill %q not found", name)
	}
	if e.exec != nil {
		out, err := e.exec.Execute(ctx, argsJSON)
		endErr = err
		return out, err
	}
	if e.sb != nil {
		out, err := e.sb.Run(ctx, argsJSON)
		endErr = err
		return out, err
	}
	endErr = fmt.Errorf("missing executor")
	return "", fmt.Errorf("skill %q has no executor (misconfigured?)", name)
}

// GetDef returns the LLM tool definition for a skill.
func (r *Registry) GetDef(name string) (llm.ToolDef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[name]
	if !ok {
		return llm.ToolDef{}, false
	}
	return e.toolDef, true
}

// AllNames returns all registered skill names.
func (r *Registry) AllNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.entries))
	for n := range r.entries {
		names = append(names, n)
	}
	return names
}

func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// ─── View — per-agent-loop progressive skill loading ─────────────────────────

// View is a per-agent-loop projection of the Registry.
// It maintains the ACTIVE skill set that is actually sent to the LLM.
// All registered skills are reachable (for execution), but only active
// ones are included in the tool definitions array sent in each LLM call.
//
// This is the DeerFlow progressive loading pattern: start with a small
// set of always-useful tools; add more only as needed.
type View struct {
	reg    *Registry
	mu     sync.Mutex
	active map[string]bool // currently included in LLM requests
	log    *slog.Logger
}

// NewView creates a view with the given eager (always-on) skill names active.
func NewView(reg *Registry, eagerSkills []string, log *slog.Logger) *View {
	v := &View{
		reg:    reg,
		active: make(map[string]bool),
		log:    log,
	}
	for _, name := range eagerSkills {
		if reg.Exists(name) {
			v.active[name] = true
		} else {
			log.Warn("eager skill not found in registry", "skill", name)
		}
	}
	return v
}

// ActiveToolDefs returns tool definitions for the current active skill set.
// This is what gets sent to the LLM in each request.
func (v *View) ActiveToolDefs() []llm.ToolDef {
	v.mu.Lock()
	defer v.mu.Unlock()
	defs := make([]llm.ToolDef, 0, len(v.active))
	for name := range v.active {
		if td, ok := v.reg.GetDef(name); ok {
			defs = append(defs, td)
		}
	}
	return defs
}

// Activate adds a skill to the active set. Idempotent.
// Returns true if the skill exists and was added.
func (v *View) Activate(name string) bool {
	if !v.reg.Exists(name) {
		return false
	}
	v.mu.Lock()
	v.active[name] = true
	v.mu.Unlock()
	v.log.Info("skill activated", "skill", name)
	telemetry.IncCounter(telemetry.MetricSkillActivations, 1, map[string]string{"skill": name})
	return true
}

// ActivateByTag activates all skills with a given tag.
func (v *View) ActivateByTag(tag string) {
	v.reg.mu.RLock()
	var toActivate []string
	for name, e := range v.reg.entries {
		if e.tags[tag] {
			toActivate = append(toActivate, name)
		}
	}
	v.reg.mu.RUnlock()

	v.mu.Lock()
	for _, name := range toActivate {
		v.active[name] = true
	}
	v.mu.Unlock()

	if len(toActivate) > 0 {
		v.log.Info("skills activated by tag", "tag", tag, "count", len(toActivate))
		telemetry.IncCounter(telemetry.MetricSkillActivations, int64(len(toActivate)), map[string]string{"tag": tag})
	}
}

// IsActive reports whether a skill is in the active set.
func (v *View) IsActive(name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.active[name]
}

// Execute executes a skill by name. Works for ALL registered skills,
// not just active ones — the LLM may request a skill that isn't active yet.
// If the skill exists but is not active, it auto-promotes it (lazy loading)
// and returns the result — the agent loop should then include it in future requests.
func (v *View) Execute(ctx context.Context, name, argsJSON string) (string, LazyLoadResult, error) {
	wasActive := v.IsActive(name)
	res, execErr := v.reg.Execute(ctx, name, argsJSON)

	if execErr == nil && !wasActive && v.reg.Exists(name) {
		// Auto-promote: the LLM found a skill that wasn't active — add it
		v.Activate(name)
		return res, LazyLoaded, nil
	}
	if execErr != nil {
		return "", ExecError, execErr
	}
	return res, AlreadyActive, nil
}

// ActiveCount returns the number of currently active skills.
func (v *View) ActiveCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.active)
}

// LazyLoadResult describes what happened during skill execution.
type LazyLoadResult int

const (
	AlreadyActive LazyLoadResult = iota // skill was already in active set
	LazyLoaded                          // skill was auto-promoted during this call
	ExecError                           // execution failed
)

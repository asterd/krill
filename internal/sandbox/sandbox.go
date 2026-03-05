// Package sandbox — isolated skill execution environments.
//
// Three runtimes:
//
//	exec  — subprocess with ephemeral tmpdir, minimal env, timeout enforced.
//	        Receives JSON on stdin, returns result on stdout.
//	        Any language, any binary. Disk is wiped after each call.
//
//	wasm  — WebAssembly via wazero (pure Go, no CGO, no external process).
//	        CPU budget via fuel. No filesystem. No syscalls.
//	        Build tag "wasm" required; stub returns error otherwise.
//
//	noop  — echoes input, for tests only.
//
// Security properties of exec sandbox:
//   - HOME, TMPDIR set to unique ephemeral dir (removed after call)
//   - Only PATH=/usr/local/bin:/usr/bin:/bin in environment
//   - No inherited file descriptors
//   - context.WithTimeout enforces wall-clock limit
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/krill/krill/internal/telemetry"
)

// Sandbox executes a skill in isolation.
type Sandbox interface {
	Run(ctx context.Context, argsJSON string) (string, error)
}

// ─── Process sandbox ──────────────────────────────────────────────────────────

type ProcessConfig struct {
	Path      string
	TimeoutMs int
	Env       map[string]string
}

type processSandbox struct{ cfg ProcessConfig }

// NewProcess creates an exec-based sandbox implementation.
func NewProcess(cfg ProcessConfig) (Sandbox, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("process sandbox: path is required")
	}
	if _, err := os.Stat(cfg.Path); err != nil {
		return nil, fmt.Errorf("process sandbox: %w", err)
	}
	return &processSandbox{cfg: cfg}, nil
}

func (p *processSandbox) Run(ctx context.Context, argsJSON string) (string, error) {
	traceID, parentSpanID, requestID := telemetry.TraceFromContext(ctx)
	span := telemetry.StartSpan(nil, traceID, parentSpanID, "sandbox.lifecycle",
		"request_id", requestID,
		"runtime", "exec",
	)
	start := time.Now()
	var endErr error
	defer func() {
		telemetry.ObserveDurationMs(telemetry.MetricSandboxExecDuration, time.Since(start), map[string]string{"runtime": "exec"})
		span.End(endErr, "request_id", requestID, "runtime", "exec")
	}()

	timeout := time.Duration(p.cfg.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Ephemeral working directory — unique per invocation, wiped after
	tmpDir, err := os.MkdirTemp("", "krill-*")
	if err != nil {
		endErr = err
		return "", fmt.Errorf("sandbox tmpdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	cmd := exec.CommandContext(runCtx, p.cfg.Path) // #nosec G204 — path from config, not user input
	cmd.Dir = tmpDir
	cmd.Stdin = strings.NewReader(argsJSON)

	// Minimal, safe environment
	cmd.Env = []string{
		"HOME=" + tmpDir,
		"TMPDIR=" + tmpDir,
		"PATH=/usr/local/bin:/usr/bin:/bin",
	}
	for k, v := range p.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := err.Error()
		if stderr.Len() > 0 {
			msg += ": " + strings.TrimSpace(stderr.String())
		}
		endErr = err
		return "", fmt.Errorf("skill exec: %s", msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ─── WASM sandbox (stub — full impl in wasm_wazero.go with build tag) ─────────

type WASMConfig struct {
	Path      string
	Fuel      uint64
	TimeoutMs int
	Env       map[string]string
}

type wasmSandbox struct{ cfg WASMConfig }

// NewWASM creates a WASM sandbox. Requires build tag "wasm" for the wazero impl.
// Without the build tag this returns a sandbox that errors on Run().
func NewWASM(cfg WASMConfig) (Sandbox, error) {
	if _, err := os.Stat(cfg.Path); err != nil {
		return nil, fmt.Errorf("wasm: file %q not found: %w", cfg.Path, err)
	}
	return &wasmSandbox{cfg: cfg}, nil
}

func (w *wasmSandbox) Run(_ context.Context, _ string) (string, error) {
	// Real implementation lives in wasm_wazero.go (build tag: wasm).
	// To enable: go build -tags wasm ./...
	// and add: github.com/tetratelabs/wazero to go.mod
	err := fmt.Errorf("wasm sandbox not compiled in (add build tag 'wasm')")
	return "", err
}

// ─── Noop sandbox (testing) ───────────────────────────────────────────────────

type noopSandbox struct{}

// NewNoop creates a sandbox that echoes inputs without execution.
func NewNoop() Sandbox { return &noopSandbox{} }

func (n *noopSandbox) Run(_ context.Context, argsJSON string) (string, error) {
	return `{"ok":true,"echo":` + argsJSON + `}`, nil
}

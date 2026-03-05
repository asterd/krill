package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNewProcessErrorsAndNoop(t *testing.T) {
	if _, err := NewProcess(ProcessConfig{}); err == nil {
		t.Fatal("expected missing path error")
	}
	if _, err := NewProcess(ProcessConfig{Path: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("expected stat error")
	}
	n := NewNoop()
	out, err := n.Run(context.Background(), `{"x":1}`)
	if err != nil || out == "" {
		t.Fatalf("noop run failed: out=%q err=%v", out, err)
	}
}

func TestProcessSandboxRunSuccessAndTimeout(t *testing.T) {
	dir := t.TempDir()
	okPath := filepath.Join(dir, "ok.sh")
	okScript := "#!/bin/sh\necho ok\n"
	if err := os.WriteFile(okPath, []byte(okScript), 0o755); err != nil {
		t.Fatal(err)
	}
	sb, err := NewProcess(ProcessConfig{Path: okPath, TimeoutMs: 0, Env: map[string]string{"X": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := sb.Run(context.Background(), `{"hello":"world"}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "ok" {
		t.Fatalf("unexpected stdout: %q", out)
	}

	slowPath := filepath.Join(dir, "slow.sh")
	slowScript := "#!/bin/sh\nsleep 1\n"
	if err := os.WriteFile(slowPath, []byte(slowScript), 0o755); err != nil {
		t.Fatal(err)
	}
	slow, err := NewProcess(ProcessConfig{Path: slowPath, TimeoutMs: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slow.Run(context.Background(), `{}`); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWASMSandboxStub(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mod.wasm")
	if err := os.WriteFile(path, []byte("wasm"), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, err := NewWASM(WASMConfig{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Run(context.Background(), `{}`); err == nil {
		t.Fatal("expected stub runtime error")
	}
}

package builtins

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestCodeExec_UnsupportedLanguage(t *testing.T) {
	e := &codeExec{}
	_, err := e.Execute(context.Background(), `{"language":"ruby","code":"puts 1"}`)
	if err == nil {
		t.Fatal("expected error for unsupported language")
	}
}

func TestCodeExec_PathTraversalBlocked(t *testing.T) {
	e := &codeExec{}
	_, err := e.Execute(context.Background(), `{
		"language":"python",
		"files":[{"path":"../hack.py","content":"print(1)"}],
		"entrypoint":"../hack.py"
	}`)
	if err == nil {
		t.Fatal("expected path traversal error")
	}
}

func TestCodeExec_PythonSuccess(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}

	e := &codeExec{}
	out, err := e.Execute(context.Background(), `{
		"language":"python",
		"code":"print(2+2)"
	}`)
	if err != nil {
		t.Fatal(err)
	}

	var res struct {
		OK       bool   `json:"ok"`
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected ok=true, got false: %s", out)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit_code=0, got %d", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "4" {
		t.Fatalf("unexpected stdout: %q", res.Stdout)
	}
}

// Package builtins registers all built-in skills into the skill registry.
// Import this package from core/engine.go; add skills here or in separate files.
package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/skill"
)

// Register adds all builtin skills to the registry.
// Called once at engine startup.
func Register(sr *skill.Registry, core config.CoreConfig) {
	// http_fetch: fetch any URL, return body text
	sr.RegisterBuiltin("http_fetch",
		"Fetch a URL and return its content. Use for reading web pages, APIs, or any HTTP resource.",
		&httpFetch{cli: &http.Client{Timeout: 30 * time.Second}},
		json.RawMessage(`{
			"type": "object",
			"required": ["url"],
			"properties": {
				"url":     {"type": "string", "description": "URL to fetch"},
				"method": {"type": "string", "enum": ["GET","POST"], "default": "GET"},
				"body":   {"type": "string", "description": "Request body for POST requests"},
				"headers": {"type": "object", "description": "Additional HTTP headers"}
			}
		}`),
	)

	// json_format: pretty-print JSON
	sr.RegisterBuiltin("json_format",
		"Parse and pretty-print a JSON string for readability.",
		&jsonFormat{},
		json.RawMessage(`{
			"type": "object",
			"required": ["input"],
			"properties": {
				"input": {"type": "string", "description": "JSON string to format"}
			}
		}`),
	)

	// text_extract: extract a field from JSON by dot-path
	sr.RegisterBuiltin("json_extract",
		"Extract a value from a JSON string using a dot-path (e.g. 'user.name').",
		&jsonExtract{},
		json.RawMessage(`{
			"type": "object",
			"required": ["json", "path"],
			"properties": {
				"json": {"type": "string", "description": "JSON string to query"},
				"path": {"type": "string", "description": "Dot-separated path, e.g. 'data.items.0.name'"}
			}
		}`),
	)

	// code_exec: run short programs inside an ephemeral workspace.
	// Designed for deterministic computations and quick code checks.
	sr.RegisterBuiltin("code_exec",
		"Execute code in an ephemeral sandbox workspace (python/javascript/bash). Returns JSON with stdout, stderr, exit code and status.",
		&codeExec{
			logCode:            core.LogGeneratedCode,
			maxLoggedCodeBytes: core.LogGeneratedCodeMaxBytes,
		},
		json.RawMessage(`{
			"type": "object",
			"required": ["language"],
			"properties": {
				"language": {"type":"string","enum":["python","javascript","bash"]},
				"code": {"type":"string","description":"Single-file source code. If files[] is omitted, this becomes main file content."},
				"files": {
					"type":"array",
					"description":"Optional multi-file workspace. Relative paths only.",
					"items": {
						"type":"object",
						"required":["path","content"],
						"properties":{
							"path":{"type":"string"},
							"content":{"type":"string"}
						}
					}
				},
				"entrypoint": {"type":"string","description":"Relative file path to execute. Defaults to main.py/main.js/main.sh."},
				"stdin": {"type":"string","description":"Optional stdin fed to the program."},
				"timeout_ms": {"type":"integer","minimum":1,"maximum":30000,"default":8000}
			}
		}`),
	)
}

// ─── http_fetch ───────────────────────────────────────────────────────────────

type httpFetch struct{ cli *http.Client }

func (s *httpFetch) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Body    string            `json:"body"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("http_fetch: %w", err)
	}
	if args.Method == "" {
		args.Method = "GET"
	}
	if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
		return "", fmt.Errorf("http_fetch: URL must start with http:// or https://")
	}

	var bodyReader io.Reader
	if args.Body != "" {
		bodyReader = strings.NewReader(args.Body)
	}
	req, err := http.NewRequestWithContext(ctx, args.Method, args.URL, bodyReader)
	if err != nil {
		return "", err
	}
	for k, v := range args.Headers {
		req.Header.Set(k, v)
	}

	resp, err := s.cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// Limit to 512KB to prevent memory exhaustion
	data, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ─── json_format ──────────────────────────────────────────────────────────────

type jsonFormat struct{}

func (s *jsonFormat) Execute(_ context.Context, argsJSON string) (string, error) {
	var args struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	var v interface{}
	if err := json.Unmarshal([]byte(args.Input), &v); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	return string(out), nil
}

// ─── json_extract ─────────────────────────────────────────────────────────────

type jsonExtract struct{}

func (s *jsonExtract) Execute(_ context.Context, argsJSON string) (string, error) {
	var args struct {
		JSON string `json:"json"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", err
	}
	var obj interface{}
	if err := json.Unmarshal([]byte(args.JSON), &obj); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}
	result := traverse(obj, strings.Split(args.Path, "."))
	out, _ := json.Marshal(result)
	return string(out), nil
}

func traverse(v interface{}, path []string) interface{} {
	if len(path) == 0 || path[0] == "" {
		return v
	}
	switch node := v.(type) {
	case map[string]interface{}:
		return traverse(node[path[0]], path[1:])
	case []interface{}:
		// numeric index support
		idx := 0
		fmt.Sscanf(path[0], "%d", &idx)
		if idx < len(node) {
			return traverse(node[idx], path[1:])
		}
	}
	return nil
}

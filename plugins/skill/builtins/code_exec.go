package builtins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/krill/krill/internal/telemetry"
)

const (
	defaultCodeTimeoutMs = 8000
	maxCodeTimeoutMs     = 30000
	maxOutputBytes       = 64 << 10
)

type codeExec struct {
	log                *slog.Logger
	logCode            bool
	maxLoggedCodeBytes int
}

func (e *codeExec) SetLogger(log *slog.Logger) {
	e.log = log
}

type codeFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type codeExecArgs struct {
	Language   string     `json:"language"`
	Code       string     `json:"code"`
	Files      []codeFile `json:"files"`
	Entrypoint string     `json:"entrypoint"`
	Stdin      string     `json:"stdin"`
	TimeoutMs  int        `json:"timeout_ms"`
}

type codeExecResult struct {
	OK         bool   `json:"ok"`
	Language   string `json:"language"`
	Entrypoint string `json:"entrypoint"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Error      string `json:"error,omitempty"`
	Truncated  bool   `json:"truncated"`
}

func (e *codeExec) Execute(ctx context.Context, argsJSON string) (out string, retErr error) {
	traceID, parentSpanID, requestID := telemetry.TraceFromContext(ctx)
	if traceID == "" {
		traceID = telemetry.NewTraceID()
	}
	span := telemetry.StartSpan(e.logger(), traceID, parentSpanID, "skill.code_exec.execute",
		"request_id", requestID,
	)
	defer func() { span.End(retErr) }()

	var args codeExecArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("code_exec: %w", err)
	}

	lang := strings.ToLower(strings.TrimSpace(args.Language))
	if lang == "" {
		return "", fmt.Errorf("code_exec: language is required")
	}

	bin, defaultEntry, err := languageSpec(lang)
	if err != nil {
		return "", err
	}

	e.logger().Info("code_exec setup",
		"trace_id", traceID,
		"request_id", requestID,
		"language", lang,
		"files_count", len(args.Files),
		"has_inline_code", strings.TrimSpace(args.Code) != "",
		"log_generated_code", e.logCode,
	)

	tmpDir, err := os.MkdirTemp("", "krill-code-*")
	if err != nil {
		return "", fmt.Errorf("code_exec: tmpdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	e.logger().Info("code_exec sandbox created",
		"trace_id", traceID,
		"request_id", requestID,
		"tmp_dir", tmpDir,
	)

	files := args.Files
	if len(files) == 0 {
		if strings.TrimSpace(args.Code) == "" {
			return "", fmt.Errorf("code_exec: provide code or files")
		}
		files = []codeFile{{Path: defaultEntry, Content: args.Code}}
	}

	for _, f := range files {
		rel, relErr := sanitizeRelativePath(f.Path)
		if relErr != nil {
			return "", fmt.Errorf("code_exec: invalid file path %q: %w", f.Path, relErr)
		}
		abs := filepath.Join(tmpDir, rel)
		if mkErr := os.MkdirAll(filepath.Dir(abs), 0o755); mkErr != nil {
			return "", fmt.Errorf("code_exec: mkdir: %w", mkErr)
		}
		if wrErr := os.WriteFile(abs, []byte(f.Content), 0o644); wrErr != nil {
			return "", fmt.Errorf("code_exec: write %q: %w", rel, wrErr)
		}
		e.logger().Info("code_exec file written",
			"trace_id", traceID,
			"request_id", requestID,
			"path", rel,
			"bytes", len(f.Content),
		)
		if e.logCode {
			preview, truncated := e.codePreview(f.Content)
			e.logger().Info("code_exec source",
				"trace_id", traceID,
				"request_id", requestID,
				"path", rel,
				"code", preview,
				"truncated", truncated,
			)
		}
	}
	if !e.logCode {
		e.logger().Info("code_exec source logging disabled",
			"trace_id", traceID,
			"request_id", requestID,
		)
	}

	entry := args.Entrypoint
	if strings.TrimSpace(entry) == "" {
		entry = defaultEntry
	}
	entry, err = sanitizeRelativePath(entry)
	if err != nil {
		return "", fmt.Errorf("code_exec: invalid entrypoint %q: %w", args.Entrypoint, err)
	}
	entryAbs := filepath.Join(tmpDir, entry)
	if _, statErr := os.Stat(entryAbs); statErr != nil {
		return "", fmt.Errorf("code_exec: entrypoint %q not found", entry)
	}

	timeoutMs := args.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = defaultCodeTimeoutMs
	}
	if timeoutMs > maxCodeTimeoutMs {
		timeoutMs = maxCodeTimeoutMs
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(runCtx, bin, entry) // #nosec G204 -- binary from fixed allowlist
	cmd.Dir = tmpDir
	cmd.Env = []string{
		"HOME=" + tmpDir,
		"TMPDIR=" + tmpDir,
		"PATH=/usr/local/bin:/usr/bin:/bin",
	}
	cmd.Stdin = strings.NewReader(args.Stdin)

	e.logger().Info("code_exec process start",
		"trace_id", traceID,
		"request_id", requestID,
		"bin", bin,
		"entrypoint", entry,
		"timeout_ms", timeoutMs,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	dur := time.Since(start)

	res := codeExecResult{
		Language:   lang,
		Entrypoint: entry,
		DurationMs: dur.Milliseconds(),
		ExitCode:   0,
		OK:         runErr == nil,
	}

	if runErr != nil {
		res.OK = false
		var exitErr *exec.ExitError
		switch {
		case errors.As(runErr, &exitErr):
			res.ExitCode = exitErr.ExitCode()
			res.Error = "process exited with non-zero status"
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			res.ExitCode = -1
			res.Error = "execution timeout"
		default:
			res.ExitCode = -1
			res.Error = runErr.Error()
		}
	}

	stdoutStr, stdoutTrunc := truncateString(stdout.String(), maxOutputBytes)
	stderrStr, stderrTrunc := truncateString(stderr.String(), maxOutputBytes)
	res.Stdout = stdoutStr
	res.Stderr = stderrStr
	res.Truncated = stdoutTrunc || stderrTrunc

	e.logger().Info("code_exec process end",
		"trace_id", traceID,
		"request_id", requestID,
		"ok", res.OK,
		"exit_code", res.ExitCode,
		"duration_ms", res.DurationMs,
		"stdout_bytes", len(res.Stdout),
		"stderr_bytes", len(res.Stderr),
		"truncated", res.Truncated,
	)

	data, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("code_exec: marshal result: %w", err)
	}
	return string(data), nil
}

func (e *codeExec) logger() *slog.Logger {
	if e.log != nil {
		return e.log
	}
	return slog.Default()
}

func sanitizeRelativePath(path string) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	clean := filepath.Clean(p)
	if clean == "." {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path traversal is not allowed")
	}
	return clean, nil
}

func languageSpec(language string) (bin string, entrypoint string, err error) {
	switch language {
	case "python", "py":
		return "python3", "main.py", nil
	case "javascript", "js", "node":
		return "node", "main.js", nil
	case "bash", "sh":
		return "bash", "main.sh", nil
	default:
		return "", "", fmt.Errorf("code_exec: unsupported language %q (use python, javascript, bash)", language)
	}
}

func truncateString(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	out := s[:maxBytes]
	return out + "\n...<truncated>", true
}

func (e *codeExec) codePreview(source string) (string, bool) {
	limit := e.maxLoggedCodeBytes
	if limit <= 0 {
		limit = 4000
	}
	return truncateString(source, limit)
}

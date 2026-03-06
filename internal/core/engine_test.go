package core

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
)

type testProtocol struct {
	name        string
	started     bool
	stopped     bool
	failOnStart bool
}

func (p *testProtocol) Name() string { return p.name }

func (p *testProtocol) Start(_ context.Context, _ bus.Bus, _ *slog.Logger) error {
	if p.failOnStart {
		return context.Canceled
	}
	p.started = true
	return nil
}

func (p *testProtocol) Stop(_ context.Context) error {
	p.stopped = true
	return nil
}

func TestCopyConfigMap(t *testing.T) {
	src := map[string]interface{}{"a": 1}
	cp := copyConfigMap(src)
	cp["a"] = 2
	if src["a"] == cp["a"] {
		t.Fatal("expected deep map copy")
	}
}

func TestNewAndRun_WithRegisteredProtocol(t *testing.T) {
	const protoName = "engine-test-proto"
	tp := &testProtocol{name: protoName}
	Global().RegisterProtocol(protoName, func(cfg map[string]interface{}) (Protocol, error) {
		if cfg["_strict_v2_validation"] != true {
			t.Fatalf("strict flag not propagated to protocol config: %+v", cfg)
		}
		return tp, nil
	})

	cfg := &config.Root{
		Core: config.CoreConfig{
			BusBuffer:                  8,
			MaxClients:                 2,
			SandboxType:                "exec",
			ReplyBusPrefix:             "__reply__",
			StrictEnvelopeV2Validation: true,
			SkillTimeoutMs:             1000,
			MemoryWindow:               10,
			LogFormat:                  "text",
			LogGeneratedCodeMaxBytes:   100,
		},
		LLM: config.LLMPool{
			Default: "mock",
			Backends: []config.LLMConfig{
				{
					Name:      "mock",
					BaseURL:   "https://example.test",
					APIKey:    "test",
					Model:     "test",
					MaxTokens: 128,
				},
			},
		},
		Protocols: []config.PluginRef{
			{Name: protoName, Enabled: true, Config: map[string]interface{}{}},
		},
	}

	e, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("engine run did not stop")
	}

	if !tp.started || !tp.stopped {
		t.Fatalf("expected protocol start+stop called, started=%v stopped=%v", tp.started, tp.stopped)
	}
}

func TestNew_FailsOnUnknownProtocolAndRunStops(t *testing.T) {
	cfg := &config.Root{
		Core: config.CoreConfig{
			BusBuffer:                  8,
			MaxClients:                 2,
			SandboxType:                "exec",
			ReplyBusPrefix:             "__reply__",
			StrictEnvelopeV2Validation: false,
			SkillTimeoutMs:             1000,
			MemoryWindow:               10,
		},
		LLM: config.LLMPool{
			Default: "mock",
			Backends: []config.LLMConfig{
				{Name: "mock", BaseURL: "https://example.test", APIKey: "test", Model: "test", MaxTokens: 128},
			},
		},
		Protocols: []config.PluginRef{{Name: "missing-proto", Enabled: true}},
	}
	if _, err := New(cfg, slog.Default()); err == nil {
		t.Fatal("expected unknown protocol error")
	}
}

func TestNew_FailsOnInvalidLLMConfig(t *testing.T) {
	cfg := &config.Root{
		Core: config.CoreConfig{BusBuffer: 8, MaxClients: 2, SandboxType: "exec"},
		LLM:  config.LLMPool{},
	}
	if _, err := New(cfg, slog.Default()); err == nil {
		t.Fatal("expected llm init error")
	}
}

func TestRun_ProtocolStartError(t *testing.T) {
	const protoName = "engine-test-proto-fail"
	Global().RegisterProtocol(protoName, func(cfg map[string]interface{}) (Protocol, error) {
		return &testProtocol{name: protoName, failOnStart: true}, nil
	})
	cfg := &config.Root{
		Core: config.CoreConfig{
			BusBuffer:      8,
			MaxClients:     2,
			SandboxType:    "exec",
			ReplyBusPrefix: "__reply__",
			SkillTimeoutMs: 1000,
			MemoryWindow:   10,
		},
		LLM: config.LLMPool{
			Default: "mock",
			Backends: []config.LLMConfig{
				{Name: "mock", BaseURL: "https://example.test", APIKey: "test", Model: "test", MaxTokens: 128},
			},
		},
		Protocols: []config.PluginRef{{Name: protoName, Enabled: true}},
	}
	e, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(context.Background()); err == nil {
		t.Fatal("expected protocol start error")
	}
}

func TestNew_WithSessionsAndScheduler(t *testing.T) {
	cfg := &config.Root{
		Core: config.CoreConfig{
			BusBuffer:      8,
			MaxClients:     2,
			SandboxType:    "exec",
			ReplyBusPrefix: "engine-reply",
			SkillTimeoutMs: 1000,
			MemoryWindow:   10,
			MemoryBackend:  "ram",
		},
		Sessions: config.SessionConfig{
			Enabled:                  true,
			Path:                     filepath.Join(t.TempDir(), "sessions.json"),
			RetentionMaxMessages:     5,
			SummarizationThreshold:   4,
			SummarizationKeepRecent:  2,
			DefaultMergeConflictMode: "last-write-wins",
		},
		Scheduler: config.SchedulerConfig{
			Enabled: true,
			TickMs:  1,
			Schedules: []config.ScheduleConfig{{
				ID:              "tick",
				CronExpr:        "* * * * *",
				Target:          "wf-scheduled",
				PayloadTemplate: "hello from schedule",
				Enabled:         true,
			}},
		},
		LLM: config.LLMPool{
			Default: "mock",
			Backends: []config.LLMConfig{
				{Name: "mock", BaseURL: "https://example.test", APIKey: "test", Model: "test", MaxTokens: 128},
			},
		},
	}
	e, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if e.sched == nil {
		t.Fatal("expected scheduler to be initialized")
	}
	if got := bus.ReplyKey("http"); got != "engine-reply:http" {
		t.Fatalf("expected configured reply prefix, got %s", got)
	}
	bus.SetReplyPrefix("__reply__")
}

func TestRun_StartsScheduler(t *testing.T) {
	cfg := &config.Root{
		Core: config.CoreConfig{
			BusBuffer:      8,
			MaxClients:     2,
			SandboxType:    "exec",
			ReplyBusPrefix: "__reply__",
			SkillTimeoutMs: 1000,
			MemoryWindow:   10,
			MemoryBackend:  "ram",
		},
		Scheduler: config.SchedulerConfig{
			Enabled: true,
			TickMs:  1,
			Schedules: []config.ScheduleConfig{{
				ID:              "tick",
				CronExpr:        "* * * * *",
				Target:          "wf-scheduled",
				PayloadTemplate: "hello from schedule",
				Enabled:         true,
			}},
		},
		LLM: config.LLMPool{
			Default: "mock",
			Backends: []config.LLMConfig{
				{Name: "mock", BaseURL: "https://example.test", APIKey: "test", Model: "test", MaxTokens: 128},
			},
		},
	}
	e, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if e.sched != nil && len(e.sched.Audit()) > 0 {
			cancel()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("engine run did not stop")
	}
	if got := len(e.sched.Audit()); got == 0 {
		t.Fatal("expected scheduler to emit audit entries during run")
	}
}

func TestRun_WithSessionsEnabledShutsDownCleanly(t *testing.T) {
	cfg := &config.Root{
		Core: config.CoreConfig{
			BusBuffer:      8,
			MaxClients:     2,
			SandboxType:    "exec",
			ReplyBusPrefix: "__reply__",
			SkillTimeoutMs: 1000,
			MemoryWindow:   10,
			MemoryBackend:  "ram",
		},
		Sessions: config.SessionConfig{
			Enabled:                  true,
			Path:                     filepath.Join(t.TempDir(), "sessions.json"),
			RetentionMaxMessages:     5,
			SummarizationThreshold:   0,
			SummarizationKeepRecent:  0,
			DefaultMergeConflictMode: "last-write-wins",
		},
		LLM: config.LLMPool{
			Default: "mock",
			Backends: []config.LLMConfig{
				{Name: "mock", BaseURL: "https://example.test", APIKey: "test", Model: "test", MaxTokens: 128},
			},
		},
	}
	e, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("engine run did not stop")
	}
}

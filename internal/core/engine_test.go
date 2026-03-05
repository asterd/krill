package core

import (
	"context"
	"log/slog"
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

// Package config — single YAML drives everything. No env-var magic except $VAR expansion.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type Root struct {
	Core      CoreConfig    `yaml:"core"`
	OTEL      OTELConfig    `yaml:"otel"`
	LLM       LLMPool       `yaml:"llm"`
	Protocols []PluginRef   `yaml:"protocols"`
	Agents    []AgentConfig `yaml:"agents"`
	Skills    []SkillConfig `yaml:"skills"`
}

type OTELConfig struct {
	Profile         string  `yaml:"profile"`           // off | minimal | standard | debug
	Exporter        string  `yaml:"exporter"`          // none | log | otlp_http
	Endpoint        string  `yaml:"endpoint"`          // OTLP HTTP endpoint base URL
	ServiceName     string  `yaml:"service_name"`      // default krill
	SampleRate      float64 `yaml:"sample_rate"`       // 0..1
	FlushIntervalMs int     `yaml:"flush_interval_ms"` // default 5000
	ConsoleDebug    bool    `yaml:"console_debug"`     // print text span-end logs for debug
}

type CoreConfig struct {
	BusBuffer      int    `yaml:"bus_buffer"`       // default 256
	MaxClients     int    `yaml:"max_clients"`      // default 1000 — semaphore for concurrent loops
	SandboxType    string `yaml:"sandbox_type"`     // "wasm" | "exec" | "noop"
	SkillTimeoutMs int    `yaml:"skill_timeout_ms"` // default 30000
	WasmFuel       uint64 `yaml:"wasm_fuel"`        // default 1_000_000_000
	MemoryWindow   int    `yaml:"memory_window"`    // max messages kept in RAM per thread, default 100
	MemoryBackend  string `yaml:"memory_backend"`   // "sqlite" | "file" | "ram" (default sqlite)
	MemoryPath     string `yaml:"memory_path"`      // used when memory_backend=sqlite|file
	LogFormat      string `yaml:"log_format"`       // "json" | "text" (default json)
	// LogGeneratedCode controls whether code_exec logs generated source code.
	LogGeneratedCode bool `yaml:"log_generated_code"` // default false
	// LogGeneratedCodeMaxBytes limits per-file code preview in logs.
	LogGeneratedCodeMaxBytes int `yaml:"log_generated_code_max_bytes"` // default 4000
	// ReplyBusKey: the bus channel key used to route replies back to protocol plugins.
	// Each protocol subscribes to "<ReplyBusKey>:<protocol_name>" and filters by ClientID in meta.
	ReplyBusPrefix string `yaml:"reply_bus_prefix"` // default "__reply__"
	// StrictEnvelopeV2Validation enforces strict schema_version=v2 validation at ingress normalization.
	StrictEnvelopeV2Validation bool `yaml:"strict_envelope_v2_validation"`
	// StrictV2Validation is a legacy alias kept for backward compatibility.
	StrictV2Validation bool `yaml:"strict_v2_validation"`
}

type PluginRef struct {
	Name    string                 `yaml:"name"`
	Enabled bool                   `yaml:"enabled"`
	Config  map[string]interface{} `yaml:"config"`
}

// AgentConfig — a named agent that the orchestrator can assign to a client.
type AgentConfig struct {
	Name         string `yaml:"name"`
	Description  string `yaml:"description"`
	LLM          string `yaml:"llm"`
	MaxTurns     int    `yaml:"max_turns"`
	SystemPrompt string `yaml:"system_prompt"`
	// Skills listed here are always loaded (eager). All others are lazy.
	EagerSkills []string `yaml:"eager_skills"`
	// Routing: if the client's source protocol matches, use this agent.
	MatchProtocol string `yaml:"match_protocol"`
}

// SkillConfig — one entry in the skill marketplace.
type SkillConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Runtime     string `yaml:"runtime"` // "builtin" | "exec" | "wasm"
	Path        string `yaml:"path"`
	// InputSchema: JSON Schema for the tool's parameters (sent to LLM).
	InputSchema string            `yaml:"input_schema"`
	Env         map[string]string `yaml:"env"`
	// Tags used for lazy loading: an agent can declare "load_skills_with_tags"
	// and only matching skills are injected when the LLM triggers them.
	Tags []string `yaml:"tags"`
}

type LLMPool struct {
	Default  string      `yaml:"default"`
	Backends []LLMConfig `yaml:"backends"`
}

type LLMConfig struct {
	Name      string `yaml:"name"`
	BaseURL   string `yaml:"base_url"`
	APIKey    string `yaml:"api_key"`
	Model     string `yaml:"model"`
	MaxTokens int    `yaml:"max_tokens"`
}

func Load(path string) (*Root, error) {
	// Optional .env fallback: load values only if not already present in the process env.
	_ = loadDotEnv(filepath.Join(filepath.Dir(path), ".env"))

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = []byte(os.ExpandEnv(string(data)))
	cfg := defaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	cfg.applyCompat()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadDotEnv reads KEY=VALUE pairs from a .env file and sets them only if
// the key is not already defined in the process environment.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}

		val := strings.TrimSpace(v)
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		_ = os.Setenv(key, val)
	}
	return sc.Err()
}

func defaults() *Root {
	return &Root{
		Core: CoreConfig{
			BusBuffer:                  256,
			MaxClients:                 1000,
			SandboxType:                "exec",
			SkillTimeoutMs:             30000,
			WasmFuel:                   1_000_000_000,
			MemoryWindow:               100,
			MemoryBackend:              "sqlite",
			MemoryPath:                 "./.krill/memory.db",
			LogFormat:                  "json",
			LogGeneratedCode:           false,
			LogGeneratedCodeMaxBytes:   4000,
			ReplyBusPrefix:             "__reply__",
			StrictEnvelopeV2Validation: false,
		},
		OTEL: OTELConfig{
			Profile:         "off",
			Exporter:        "none",
			ServiceName:     "krill",
			SampleRate:      1.0,
			FlushIntervalMs: 5000,
			ConsoleDebug:    false,
		},
	}
}

func (c *Root) applyCompat() {
	if c.Core.StrictV2Validation {
		c.Core.StrictEnvelopeV2Validation = true
	}
}

func (c *Root) validate() error {
	if !slices.Contains([]string{"exec", "wasm", "noop"}, strings.ToLower(strings.TrimSpace(c.Core.SandboxType))) {
		return fmt.Errorf("core.sandbox_type must be one of exec|wasm|noop")
	}
	if !slices.Contains([]string{"sqlite", "file", "ram"}, strings.ToLower(strings.TrimSpace(c.Core.MemoryBackend))) {
		return fmt.Errorf("core.memory_backend must be one of sqlite|file|ram")
	}
	if !slices.Contains([]string{"off", "minimal", "standard", "debug"}, strings.ToLower(strings.TrimSpace(c.OTEL.Profile))) {
		return fmt.Errorf("otel.profile must be one of off|minimal|standard|debug")
	}
	if !slices.Contains([]string{"none", "log", "otlp_http"}, strings.ToLower(strings.TrimSpace(c.OTEL.Exporter))) {
		return fmt.Errorf("otel.exporter must be one of none|log|otlp_http")
	}
	if c.OTEL.SampleRate < 0 || c.OTEL.SampleRate > 1 {
		return fmt.Errorf("otel.sample_rate must be in range [0,1]")
	}
	seen := make(map[string]struct{}, len(c.Protocols))
	for _, p := range c.Protocols {
		name := strings.ToLower(strings.TrimSpace(p.Name))
		if name == "" {
			return fmt.Errorf("protocol name is required")
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("protocol %q configured more than once", name)
		}
		seen[name] = struct{}{}
		if err := validateProtocol(name, p); err != nil {
			return err
		}
	}
	return nil
}

func validateProtocol(name string, p PluginRef) error {
	switch name {
	case "http":
		return nil
	case "pubsub":
		if !p.Enabled {
			return nil
		}
		broker, _ := p.Config["broker"].(string)
		broker = strings.ToLower(strings.TrimSpace(broker))
		if broker == "" {
			broker = "nats"
		}
		if !slices.Contains([]string{"nats", "redis_streams", "solace"}, broker) {
			return fmt.Errorf("protocol %q requires config.broker in nats|redis_streams|solace", name)
		}
		topicIn, _ := p.Config["topic_in"].(string)
		topicOut, _ := p.Config["topic_out"].(string)
		if strings.TrimSpace(topicIn) == "" || strings.TrimSpace(topicOut) == "" {
			return fmt.Errorf("protocol %q requires config.topic_in and config.topic_out when enabled", name)
		}
		return nil
	case "telegram":
		if !p.Enabled {
			return nil
		}
		token, _ := p.Config["token"].(string)
		if strings.TrimSpace(token) == "" {
			return fmt.Errorf("protocol %q requires config.token when enabled", name)
		}
		return nil
	case "webhook":
		if !p.Enabled {
			return nil
		}
		path, _ := p.Config["path"].(string)
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("protocol %q requires config.path starting with '/'", name)
		}
		return nil
	default:
		return fmt.Errorf("protocol %q is not in compatibility matrix (http|pubsub|telegram|webhook)", name)
	}
}

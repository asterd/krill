// Package config — single YAML drives everything. No env-var magic except $VAR expansion.
package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Root struct {
	Core      CoreConfig    `yaml:"core"`
	LLM       LLMPool       `yaml:"llm"`
	Protocols []PluginRef   `yaml:"protocols"`
	Agents    []AgentConfig `yaml:"agents"`
	Skills    []SkillConfig `yaml:"skills"`
}

type CoreConfig struct {
	BusBuffer      int    `yaml:"bus_buffer"`       // default 256
	MaxClients     int    `yaml:"max_clients"`      // default 1000 — semaphore for concurrent loops
	SandboxType    string `yaml:"sandbox_type"`     // "wasm" | "exec" | "noop"
	SkillTimeoutMs int    `yaml:"skill_timeout_ms"` // default 30000
	WasmFuel       uint64 `yaml:"wasm_fuel"`        // default 1_000_000_000
	MemoryWindow   int    `yaml:"memory_window"`    // max messages kept in RAM per thread, default 100
	LogFormat      string `yaml:"log_format"`       // "json" | "text" (default json)
	// LogGeneratedCode controls whether code_exec logs generated source code.
	LogGeneratedCode bool `yaml:"log_generated_code"` // default false
	// LogGeneratedCodeMaxBytes limits per-file code preview in logs.
	LogGeneratedCodeMaxBytes int `yaml:"log_generated_code_max_bytes"` // default 4000
	// ReplyBusKey: the bus channel key used to route replies back to protocol plugins.
	// Each protocol subscribes to "<ReplyBusKey>:<protocol_name>" and filters by ClientID in meta.
	ReplyBusPrefix string `yaml:"reply_bus_prefix"` // default "__reply__"
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
	return cfg, yaml.Unmarshal(data, cfg)
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
			BusBuffer:                256,
			MaxClients:               1000,
			SandboxType:              "exec",
			SkillTimeoutMs:           30000,
			WasmFuel:                 1_000_000_000,
			MemoryWindow:             100,
			LogFormat:                "json",
			LogGeneratedCode:         false,
			LogGeneratedCodeMaxBytes: 4000,
			ReplyBusPrefix:           "__reply__",
		},
	}
}

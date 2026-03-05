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
	Core       CoreConfig        `yaml:"core"`
	OTEL       OTELConfig        `yaml:"otel"`
	LLM        LLMPool           `yaml:"llm"`
	Sessions   SessionConfig     `yaml:"sessions"`
	Scheduler  SchedulerConfig   `yaml:"scheduler"`
	Protocols  []PluginRef       `yaml:"protocols"`
	Agents     []AgentConfig     `yaml:"agents"`
	Workflows  []WorkflowConfig  `yaml:"workflows"`
	OrgSchemas []OrgSchemaConfig `yaml:"org_schemas"`
	Skills     []SkillConfig     `yaml:"skills"`
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

type WorkflowConfig struct {
	ID                string               `yaml:"id"`
	OrchestrationMode string               `yaml:"orchestration_mode"` // single | cooperative
	OrgSchema         string               `yaml:"org_schema"`
	Policy            WorkflowPolicyConfig `yaml:"policy"`
}

type WorkflowPolicyConfig struct {
	MaxHops       int      `yaml:"max_hops"`
	AllowedPairs  []string `yaml:"allowed_pairs"` // "origin->target"
	StepTimeoutMs int      `yaml:"step_timeout_ms"`
	TokenBudget   int      `yaml:"token_budget"`
}

type OrgSchemaConfig struct {
	SchemaID        string                    `yaml:"schema_id"`
	Version         string                    `yaml:"version"`
	Roles           []OrgRoleConfig           `yaml:"roles"`
	HandoffRules    []OrgHandoffRuleConfig    `yaml:"handoff_rules"`
	EscalationRules []OrgEscalationRuleConfig `yaml:"escalation_rules"`
}

type OrgRoleConfig struct {
	Name             string   `yaml:"name"`
	Kind             string   `yaml:"kind"` // router | specialist | synthesizer | custom
	Agent            string   `yaml:"agent"`
	Responsibilities []string `yaml:"responsibilities"`
}

type OrgHandoffRuleConfig struct {
	From string   `yaml:"from"`
	To   []string `yaml:"to"`
}

type OrgEscalationRuleConfig struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
	When string `yaml:"when"`
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

type SessionConfig struct {
	Enabled                  bool   `yaml:"enabled"`
	Path                     string `yaml:"path"`
	ResumeOnInbound          bool   `yaml:"resume_on_inbound"`
	RetentionMaxMessages     int    `yaml:"retention_max_messages"`
	SummarizationThreshold   int    `yaml:"summarization_threshold"`
	SummarizationKeepRecent  int    `yaml:"summarization_keep_recent"`
	DefaultMergeConflictMode string `yaml:"default_merge_conflict_mode"`
}

type SchedulerConfig struct {
	Enabled   bool             `yaml:"enabled"`
	TickMs    int              `yaml:"tick_ms"`
	Schedules []ScheduleConfig `yaml:"schedules"`
}

type ScheduleConfig struct {
	ID                string `yaml:"schedule_id"`
	CronExpr          string `yaml:"cron_expr"`
	Timezone          string `yaml:"timezone"`
	Target            string `yaml:"target"`
	PayloadTemplate   string `yaml:"payload_template"`
	ConcurrencyPolicy string `yaml:"concurrency_policy"`
	Enabled           bool   `yaml:"enabled"`
	MissedRunPolicy   string `yaml:"missed_run_policy"`
	RetryLimit        int    `yaml:"retry_limit"`
	RetryBackoffMs    int    `yaml:"retry_backoff_ms"`
	SessionMode       string `yaml:"session_mode"`
	Tenant            string `yaml:"tenant"`
	ClientID          string `yaml:"client_id"`
	ThreadID          string `yaml:"thread_id"`
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
		Sessions: SessionConfig{
			Enabled:                  false,
			Path:                     "./.krill/sessions.json",
			ResumeOnInbound:          true,
			RetentionMaxMessages:     200,
			SummarizationThreshold:   24,
			SummarizationKeepRecent:  8,
			DefaultMergeConflictMode: "last-write-wins",
		},
		Scheduler: SchedulerConfig{
			Enabled: false,
			TickMs:  1000,
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
	if err := c.validateOrgSchemas(); err != nil {
		return err
	}
	if err := c.validateWorkflows(); err != nil {
		return err
	}
	if err := c.validateSessions(); err != nil {
		return err
	}
	if err := c.validateScheduler(); err != nil {
		return err
	}
	return nil
}

func (c *Root) validateSessions() error {
	mode := strings.ToLower(strings.TrimSpace(c.Sessions.DefaultMergeConflictMode))
	if mode == "" {
		mode = "last-write-wins"
	}
	if !slices.Contains([]string{"fail", "last-write-wins", "manual"}, mode) {
		return fmt.Errorf("sessions.default_merge_conflict_mode must be fail|last-write-wins|manual")
	}
	if c.Sessions.RetentionMaxMessages < 0 {
		return fmt.Errorf("sessions.retention_max_messages must be >= 0")
	}
	if c.Sessions.SummarizationThreshold < 0 {
		return fmt.Errorf("sessions.summarization_threshold must be >= 0")
	}
	if c.Sessions.SummarizationKeepRecent < 0 {
		return fmt.Errorf("sessions.summarization_keep_recent must be >= 0")
	}
	if c.Sessions.SummarizationThreshold > 0 && c.Sessions.SummarizationKeepRecent >= c.Sessions.SummarizationThreshold {
		return fmt.Errorf("sessions.summarization_keep_recent must be < sessions.summarization_threshold")
	}
	return nil
}

func (c *Root) validateScheduler() error {
	if c.Scheduler.TickMs < 0 {
		return fmt.Errorf("scheduler.tick_ms must be >= 0")
	}
	seen := make(map[string]struct{}, len(c.Scheduler.Schedules))
	for _, s := range c.Scheduler.Schedules {
		id := strings.TrimSpace(s.ID)
		if id == "" {
			return fmt.Errorf("scheduler.schedules.schedule_id is required")
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("scheduler schedule %q configured more than once", id)
		}
		seen[id] = struct{}{}
		if strings.TrimSpace(s.CronExpr) == "" {
			return fmt.Errorf("scheduler schedule %q requires cron_expr", id)
		}
		if strings.TrimSpace(s.Target) == "" {
			return fmt.Errorf("scheduler schedule %q requires target", id)
		}
		policy := strings.ToLower(strings.TrimSpace(s.ConcurrencyPolicy))
		if policy == "" {
			policy = "allow"
		}
		if !slices.Contains([]string{"allow", "forbid", "replace"}, policy) {
			return fmt.Errorf("scheduler schedule %q concurrency_policy must be allow|forbid|replace", id)
		}
		missed := strings.ToLower(strings.TrimSpace(s.MissedRunPolicy))
		if missed == "" {
			missed = "skip"
		}
		if !slices.Contains([]string{"skip", "run_once"}, missed) {
			return fmt.Errorf("scheduler schedule %q missed_run_policy must be skip|run_once", id)
		}
		if s.RetryLimit < 0 {
			return fmt.Errorf("scheduler schedule %q retry_limit must be >= 0", id)
		}
		if s.RetryBackoffMs < 0 {
			return fmt.Errorf("scheduler schedule %q retry_backoff_ms must be >= 0", id)
		}
		mode := strings.ToLower(strings.TrimSpace(s.SessionMode))
		if mode == "" {
			mode = "persistent"
		}
		if !slices.Contains([]string{"ephemeral", "persistent"}, mode) {
			return fmt.Errorf("scheduler schedule %q session_mode must be ephemeral|persistent", id)
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
	case "a2a":
		if !p.Enabled {
			return nil
		}
		path, _ := p.Config["path"].(string)
		if path != "" && !strings.HasPrefix(path, "/") {
			return fmt.Errorf("protocol %q requires config.path starting with '/'", name)
		}
		return nil
	default:
		return fmt.Errorf("protocol %q is not in compatibility matrix (http|pubsub|telegram|webhook|a2a)", name)
	}
}

func (c *Root) validateOrgSchemas() error {
	if len(c.OrgSchemas) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(c.OrgSchemas))
	agentSet := make(map[string]struct{}, len(c.Agents))
	for _, a := range c.Agents {
		agentSet[strings.TrimSpace(a.Name)] = struct{}{}
	}
	for _, s := range c.OrgSchemas {
		id := strings.TrimSpace(s.SchemaID)
		if id == "" {
			return fmt.Errorf("org_schemas.schema_id is required")
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("org_schemas %q configured more than once", id)
		}
		seen[id] = struct{}{}
		if len(s.Roles) == 0 {
			return fmt.Errorf("org_schema %q requires at least one role", id)
		}

		roleSet := make(map[string]struct{}, len(s.Roles))
		routerCount := 0
		synthCount := 0
		specCount := 0
		for _, r := range s.Roles {
			rName := strings.TrimSpace(r.Name)
			rKind := strings.ToLower(strings.TrimSpace(r.Kind))
			rAgent := strings.TrimSpace(r.Agent)
			if rName == "" || rKind == "" || rAgent == "" {
				return fmt.Errorf("org_schema %q role requires name/kind/agent", id)
			}
			if _, ok := roleSet[rName]; ok {
				return fmt.Errorf("org_schema %q role %q duplicated", id, rName)
			}
			roleSet[rName] = struct{}{}
			if _, ok := agentSet[rAgent]; !ok {
				return fmt.Errorf("org_schema %q role %q references unknown agent %q", id, rName, rAgent)
			}
			if !slices.Contains([]string{"router", "specialist", "synthesizer", "custom"}, rKind) {
				return fmt.Errorf("org_schema %q role %q has unsupported kind %q", id, rName, rKind)
			}
			if rKind == "router" {
				routerCount++
			}
			if rKind == "synthesizer" {
				synthCount++
			}
			if rKind == "specialist" {
				specCount++
			}
		}
		if routerCount != 1 {
			return fmt.Errorf("org_schema %q must define exactly one router role", id)
		}
		if synthCount != 1 {
			return fmt.Errorf("org_schema %q must define exactly one synthesizer role", id)
		}
		if specCount < 1 {
			return fmt.Errorf("org_schema %q must define at least one specialist role", id)
		}
		for _, rule := range s.HandoffRules {
			from := strings.TrimSpace(rule.From)
			if _, ok := roleSet[from]; !ok {
				return fmt.Errorf("org_schema %q handoff rule references unknown from role %q", id, from)
			}
			if len(rule.To) == 0 {
				return fmt.Errorf("org_schema %q handoff rule from %q must include at least one destination", id, from)
			}
			for _, to := range rule.To {
				if _, ok := roleSet[strings.TrimSpace(to)]; !ok {
					return fmt.Errorf("org_schema %q handoff rule references unknown target role %q", id, to)
				}
			}
		}
		for _, esc := range s.EscalationRules {
			if _, ok := roleSet[strings.TrimSpace(esc.From)]; !ok {
				return fmt.Errorf("org_schema %q escalation rule references unknown from role %q", id, esc.From)
			}
			if _, ok := roleSet[strings.TrimSpace(esc.To)]; !ok {
				return fmt.Errorf("org_schema %q escalation rule references unknown target role %q", id, esc.To)
			}
		}
	}
	return nil
}

func (c *Root) validateWorkflows() error {
	if len(c.Workflows) == 0 {
		return nil
	}
	orgSet := make(map[string]struct{}, len(c.OrgSchemas))
	for _, s := range c.OrgSchemas {
		orgSet[strings.TrimSpace(s.SchemaID)] = struct{}{}
	}
	seen := make(map[string]struct{}, len(c.Workflows))
	for _, wf := range c.Workflows {
		id := strings.TrimSpace(wf.ID)
		mode := strings.ToLower(strings.TrimSpace(wf.OrchestrationMode))
		if id == "" {
			return fmt.Errorf("workflows.id is required")
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("workflow %q configured more than once", id)
		}
		seen[id] = struct{}{}
		if mode == "" {
			mode = "single"
		}
		if !slices.Contains([]string{"single", "cooperative"}, mode) {
			return fmt.Errorf("workflow %q orchestration_mode must be single|cooperative", id)
		}
		if mode == "cooperative" {
			org := strings.TrimSpace(wf.OrgSchema)
			if org == "" {
				return fmt.Errorf("workflow %q in cooperative mode requires org_schema", id)
			}
			if _, ok := orgSet[org]; !ok {
				return fmt.Errorf("workflow %q references unknown org_schema %q", id, org)
			}
		}
	}
	return nil
}

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_CompatLegacyStrictFlag(t *testing.T) {
	cfg := `
core:
  sandbox_type: exec
  strict_v2_validation: true
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: http
    enabled: true
    config: {}
`
	path := writeTempConfig(t, cfg)
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Core.StrictEnvelopeV2Validation {
		t.Fatal("expected strict_envelope_v2_validation=true from legacy alias")
	}
}

func TestLoad_RejectUnknownProtocolCompatibilityMatrix(t *testing.T) {
	cfg := `
core:
  sandbox_type: exec
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: ghost
    enabled: true
    config: {}
`
	path := writeTempConfig(t, cfg)
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error for unsupported protocol")
	}
}

func TestLoad_AcceptsPubsubProtocol(t *testing.T) {
	cfg := `
core:
  sandbox_type: exec
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: pubsub
    enabled: true
    config:
      broker: nats
      topic_in: krill.in
      topic_out: krill.out
`
	if _, err := Load(writeTempConfig(t, cfg)); err != nil {
		t.Fatalf("expected valid pubsub config, got: %v", err)
	}
}

func TestLoad_RejectPubsubMissingTopics(t *testing.T) {
	cfg := `
core:
  sandbox_type: exec
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: pubsub
    enabled: true
    config:
      broker: nats
`
	if _, err := Load(writeTempConfig(t, cfg)); err == nil {
		t.Fatal("expected pubsub topic validation error")
	}
}

func TestLoad_RejectEnabledTelegramWithoutToken(t *testing.T) {
	cfg := `
core:
  sandbox_type: exec
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: telegram
    enabled: true
    config:
      poll_ms: 1000
`
	path := writeTempConfig(t, cfg)
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error for missing telegram token")
	}
}

func TestLoadDotEnv_ParsesAndDoesNotOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "A=1\nexport B=2\n# comment\nC='3'\nD=\"4\"\nINVALID\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("B", "keep")
	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}

	if v := os.Getenv("A"); v != "1" {
		t.Fatalf("expected A=1, got %q", v)
	}
	if v := os.Getenv("B"); v != "keep" {
		t.Fatalf("expected B not overridden, got %q", v)
	}
	if v := os.Getenv("C"); v != "3" {
		t.Fatalf("expected C=3, got %q", v)
	}
	if v := os.Getenv("D"); v != "4" {
		t.Fatalf("expected D=4, got %q", v)
	}
}

func TestValidateProtocolPathsAndDuplicates(t *testing.T) {
	cfg := `
core:
  sandbox_type: exec
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: webhook
    enabled: true
    config:
      path: webhook
  - name: webhook
    enabled: false
    config:
      path: /webhook
`
	path := writeTempConfig(t, cfg)
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error for duplicate protocol/path")
	}
}

func TestLoad_RejectInvalidSandbox(t *testing.T) {
	cfg := `
core:
  sandbox_type: bad
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: http
    enabled: true
    config: {}
`
	path := writeTempConfig(t, cfg)
	if _, err := Load(path); err == nil {
		t.Fatal("expected sandbox validation error")
	}
}

func TestLoad_RejectInvalidMemoryBackend(t *testing.T) {
	cfg := `
core:
  sandbox_type: exec
  memory_backend: bad
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: http
    enabled: true
    config: {}
`
	if _, err := Load(writeTempConfig(t, cfg)); err == nil {
		t.Fatal("expected memory backend validation error")
	}
}

func TestLoad_NonRegression_LegacyProtocolsStillValid(t *testing.T) {
	cfg := `
core:
  sandbox_type: exec
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: http
    enabled: true
    config:
      addr: ":8080"
  - name: telegram
    enabled: true
    config:
      token: token
      poll_ms: 1000
  - name: webhook
    enabled: true
    config:
      path: /webhook
`
	if _, err := Load(writeTempConfig(t, cfg)); err != nil {
		t.Fatalf("legacy protocols should remain valid: %v", err)
	}
}

func TestLoad_DefaultsMemoryBackendSQLite(t *testing.T) {
	cfg := `
core:
  sandbox_type: exec
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: http
    enabled: true
    config: {}
`
	got, err := Load(writeTempConfig(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if got.Core.MemoryBackend != "sqlite" {
		t.Fatalf("expected default memory_backend=sqlite, got %q", got.Core.MemoryBackend)
	}
}

func TestLoad_DefaultsOTELOff(t *testing.T) {
	cfg := `
core:
  sandbox_type: exec
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: http
    enabled: true
    config: {}
`
	got, err := Load(writeTempConfig(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if got.OTEL.Profile != "off" || got.OTEL.Exporter != "none" {
		t.Fatalf("unexpected OTEL defaults: %+v", got.OTEL)
	}
}

func TestLoad_RejectInvalidOTELProfile(t *testing.T) {
	cfg := `
core:
  sandbox_type: exec
otel:
  profile: wrong
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: http
    enabled: true
    config: {}
`
	if _, err := Load(writeTempConfig(t, cfg)); err == nil {
		t.Fatal("expected otel.profile validation error")
	}
}

func TestLoad_RejectInvalidOTELSampleRate(t *testing.T) {
	cfg := `
core:
  sandbox_type: exec
otel:
  profile: standard
  sample_rate: 1.5
llm:
  default: gpt4o
  backends:
    - name: gpt4o
      base_url: https://example.test
      api_key: test
      model: x
      max_tokens: 1
protocols:
  - name: http
    enabled: true
    config: {}
`
	if _, err := Load(writeTempConfig(t, cfg)); err == nil {
		t.Fatal("expected otel.sample_rate validation error")
	}
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "krill.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

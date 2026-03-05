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
  - name: pubsub
    enabled: true
    config: {}
`
	path := writeTempConfig(t, cfg)
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error for unsupported protocol")
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

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "krill.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

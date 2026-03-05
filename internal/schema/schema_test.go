package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krill/krill/internal/bus"
)

func TestDefaultV2_Defaulting(t *testing.T) {
	v2 := DefaultV2(EnvelopeV2{
		ClientID:       "c1",
		SourceProtocol: "http",
		Role:           "user",
		Text:           "hello",
	})
	if v2.SchemaVersion != VersionV2 {
		t.Fatalf("expected schema_version %q, got %q", VersionV2, v2.SchemaVersion)
	}
	if v2.ThreadID != "c1" {
		t.Fatalf("expected thread default to client_id, got %q", v2.ThreadID)
	}
	if v2.Tenant != "default" || v2.WorkflowID != "default" {
		t.Fatalf("expected tenant/workflow defaults, got tenant=%q workflow=%q", v2.Tenant, v2.WorkflowID)
	}
	if v2.CreatedAt.IsZero() {
		t.Fatal("expected created_at default")
	}
}

func TestNormalizeJSON_InvalidSchemaRejected(t *testing.T) {
	_, err := NormalizeJSON([]byte(`{"schema_version":"v9","id":"1"}`), NormalizeOptions{StrictV2Validation: true})
	if err == nil || !strings.Contains(err.Error(), "unsupported schema_version") {
		t.Fatalf("expected unsupported schema error, got %v", err)
	}
}

func TestNormalizeJSON_StrictModeRejectsInvalidV2(t *testing.T) {
	_, err := NormalizeJSON([]byte(`{"schema_version":"v2","id":"1","source_protocol":"http","role":"user","text":"x"}`), NormalizeOptions{StrictV2Validation: true})
	if err == nil || !strings.Contains(err.Error(), "client_id is required") {
		t.Fatalf("expected strict validation error, got %v", err)
	}
}

func TestBusMapping_Roundtrip(t *testing.T) {
	now := time.Date(2026, 3, 5, 11, 0, 0, 0, time.UTC)
	in := &bus.Envelope{
		ID:             "id-bus",
		ClientID:       "c1",
		ThreadID:       "t1",
		Role:           bus.RoleUser,
		Text:           "hello",
		SourceProtocol: "http",
		Meta:           map[string]string{"trace_id": "1"},
		CreatedAt:      now,
	}
	v2 := DefaultV2(BusToV2(in))
	if err := ValidateV2(v2, true); err != nil {
		t.Fatal(err)
	}
	out := V2ToBus(v2)
	if out.ID != in.ID || out.ClientID != in.ClientID || out.SourceProtocol != in.SourceProtocol {
		t.Fatalf("bus mapping mismatch: in=%+v out=%+v", in, out)
	}
	if out.Meta["schema_version"] != "v2" {
		t.Fatalf("schema_version metadata missing: %+v", out.Meta)
	}
}

func TestValidateV2_MissingFields(t *testing.T) {
	base := EnvelopeV2{
		SchemaVersion:  "v2",
		ID:             "1",
		ClientID:       "c1",
		ThreadID:       "t1",
		Tenant:         "default",
		WorkflowID:     "wf",
		SourceProtocol: "http",
		Role:           "user",
		CreatedAt:      time.Now().UTC(),
	}
	cases := []struct {
		name string
		mut  func(*EnvelopeV2)
	}{
		{"id", func(v *EnvelopeV2) { v.ID = "" }},
		{"client_id", func(v *EnvelopeV2) { v.ClientID = "" }},
		{"thread_id", func(v *EnvelopeV2) { v.ThreadID = "" }},
		{"source_protocol", func(v *EnvelopeV2) { v.SourceProtocol = "" }},
		{"role", func(v *EnvelopeV2) { v.Role = "" }},
		{"tenant", func(v *EnvelopeV2) { v.Tenant = "" }},
		{"workflow_id", func(v *EnvelopeV2) { v.WorkflowID = "" }},
		{"created_at", func(v *EnvelopeV2) { v.CreatedAt = time.Time{} }},
	}
	for _, tc := range cases {
		v := base
		tc.mut(&v)
		if err := ValidateV2(v, false); err == nil {
			t.Fatalf("expected validation error for %s", tc.name)
		}
	}
}

func TestNormalizeJSON_GoldenV2Deterministic(t *testing.T) {
	in := readTestFile(t, "testdata/v2_payload.json")
	want := compactJSON(t, readTestFile(t, "testdata/v2_expected_v2.json"))
	gotV2, err := NormalizeJSON(in, NormalizeOptions{StrictV2Validation: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(gotV2)
	if err != nil {
		t.Fatal(err)
	}
	if compactJSON(t, got) != want {
		t.Fatalf("golden mismatch\nwant=%s\ngot=%s", want, compactJSON(t, got))
	}
}

func BenchmarkNormalizeV2(b *testing.B) {
	payload := []byte(`{"schema_version":"v2","id":"v2-001","client_id":"c1","thread_id":"t1","tenant":"default","workflow_id":"default","source_protocol":"http","role":"user","text":"hello","created_at":"2026-03-05T10:01:00Z"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NormalizeJSON(payload, NormalizeOptions{StrictV2Validation: true}); err != nil {
			b.Fatal(err)
		}
	}
}

func readTestFile(t *testing.T, rel string) []byte {
	t.Helper()
	path := filepath.Clean(rel)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func compactJSON(t *testing.T, b []byte) string {
	t.Helper()
	var tmp map[string]interface{}
	if err := json.Unmarshal(b, &tmp); err != nil {
		t.Fatalf("compactJSON: %v", err)
	}
	out, err := json.Marshal(tmp)
	if err != nil {
		t.Fatalf("compactJSON marshal: %v", err)
	}
	return string(out)
}

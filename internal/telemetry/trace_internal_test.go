package telemetry

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestNormalizeConfigAndHelpers(t *testing.T) {
	cfg := normalizeConfig(Config{Profile: "standard"})
	if cfg.Exporter == "" || cfg.ServiceName == "" || cfg.SampleRate <= 0 {
		t.Fatalf("unexpected normalized config: %+v", cfg)
	}
	if !isMinimalSpan("agent.turn") || isMinimalSpan("memory.append") {
		t.Fatal("minimal span allowlist mismatch")
	}
	if !remoteParent(NewTraceID(), NewSpanID()).IsValid() {
		t.Fatal("expected valid remote parent span context")
	}
}

func TestConfigureAndResetHelpers(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	Configure(Config{Profile: "debug", Exporter: "none", ServiceName: "krill-test"}, log)
	defer Shutdown(context.Background())

	if !enabled() {
		t.Fatal("telemetry should be enabled")
	}
	if currentProfile() != ProfileDebug {
		t.Fatalf("unexpected profile: %s", currentProfile())
	}
	if NewTraceID() == "" || NewSpanID() == "" {
		t.Fatal("expected generated ids")
	}

	span := StartSpan(nil, "", "", "orchestrator.route")
	if span.TraceID() == "" || span.SpanID() == "" {
		t.Fatal("expected span ids")
	}
	span.End(nil)
	spanErr := StartSpan(nil, span.TraceID(), span.SpanID(), "llm.call")
	spanErr.End(context.DeadlineExceeded, "phase", "complete")

	// Cover histogram/counter emission path under enabled profile.
	IncCounter(MetricMemoryOpsTotal, 2, map[string]string{"op": "append"})
	SetGauge(MetricActiveLoops, 3, nil)
	ObserveDurationMs(MetricSandboxExecDuration, 10*time.Millisecond, map[string]string{"runtime": "exec"})

	ResetForTests()
	if currentProfile() != ProfileOff {
		t.Fatalf("expected off profile after reset, got %s", currentProfile())
	}
}

func TestConfigureExporterVariantsAndNoopSpan(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// log exporter path
	Configure(Config{Profile: "debug", Exporter: "log", ServiceName: "krill-test"}, log)
	Shutdown(context.Background())

	// otlp exporter path with invalid endpoint should not panic
	Configure(Config{Profile: "standard", Exporter: "otlp_http", Endpoint: "://bad", ServiceName: "krill-test"}, log)
	Shutdown(context.Background())

	// profile off -> noop span branch
	Configure(Config{Profile: "off", Exporter: "none", ServiceName: "krill-test"}, log)
	defer Shutdown(context.Background())
	s := StartSpan(nil, "", "", "agent.turn")
	if s.enabled {
		t.Fatal("expected noop span when profile off")
	}
	if s.TraceID() != "" {
		t.Fatal("expected empty trace id for noop span")
	}
	s.End(nil)
}

func TestNilHelpers(t *testing.T) {
	var s *Span
	if s.TraceID() != "" || s.SpanID() != "" {
		t.Fatal("nil span ids should be empty")
	}
	if out := SnapshotMetrics(); out != nil {
		t.Fatal("snapshot should be nil when telemetry not configured")
	}
}

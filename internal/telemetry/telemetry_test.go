package telemetry

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
)

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.MultiWriter(buf), &slog.HandlerOptions{}))
}

func TestTraceContextPropagation(t *testing.T) {
	ctx := WithTrace(context.Background(), "trace-1", "span-1", "req-1")
	traceID, spanID, requestID := TraceFromContext(ctx)
	if traceID != "trace-1" || spanID != "span-1" || requestID != "req-1" {
		t.Fatalf("unexpected context values: %q %q %q", traceID, spanID, requestID)
	}
}

func TestMetricEmissionGuards(t *testing.T) {
	Configure(Config{Profile: "standard", Exporter: "none", ServiceName: "krill-test"}, testLogger(&bytes.Buffer{}))
	defer Shutdown(context.Background())

	IncCounter(MetricMemoryOpsTotal, 1, map[string]string{"op": "append"})
	IncCounter("krill.unknown.metric", 1, nil)
	snap := SnapshotMetrics()

	var hasKnown bool
	for _, sample := range snap {
		if sample.Name == MetricMemoryOpsTotal && sample.Value > 0 {
			hasKnown = true
		}
		if sample.Name == "krill.unknown.metric" {
			t.Fatalf("unknown metric must not be emitted")
		}
	}
	if !hasKnown {
		t.Fatalf("expected known metric sample")
	}
}

func TestMetricStoreUpsertBranches(t *testing.T) {
	store := newMetricStore()
	preRegisterCoreMetrics(store)

	store.upsert(metricSample{Name: MetricMemoryOpsTotal, Type: string(metricCounter), Value: 1})
	store.upsert(metricSample{Name: MetricMemoryOpsTotal, Type: string(metricCounter), Value: 2})
	store.upsert(metricSample{Name: MetricSandboxExecDuration, Type: string(metricHist), Value: 3, Count: 1, Sum: 3})
	store.upsert(metricSample{Name: MetricSandboxExecDuration, Type: string(metricHist), Value: 5, Count: 1, Sum: 5})
	store.upsert(metricSample{Name: "krill.unknown", Type: string(metricCounter), Value: 99})

	var counterFound, histFound bool
	for _, s := range store.snapshot() {
		if s.Name == MetricMemoryOpsTotal && s.Value >= 3 {
			counterFound = true
		}
		if s.Name == MetricSandboxExecDuration && s.Count >= 2 && s.Sum >= 8 {
			histFound = true
		}
		if s.Name == "krill.unknown" {
			t.Fatal("unknown metric should be dropped")
		}
	}
	if !counterFound || !histFound {
		t.Fatal("expected aggregated counter and histogram samples")
	}
}

func TestProfileOffDropsMetrics(t *testing.T) {
	Configure(Config{Profile: "off", Exporter: "none", ServiceName: "krill-test"}, testLogger(&bytes.Buffer{}))
	defer Shutdown(context.Background())

	IncCounter(MetricMemoryOpsTotal, 1, nil)
	for _, sample := range SnapshotMetrics() {
		if sample.Name == MetricMemoryOpsTotal && sample.Value > 0 {
			t.Fatalf("expected no increments when profile is off")
		}
	}
}

func TestConsoleDebugSpanEndLog(t *testing.T) {
	buf := bytes.Buffer{}
	Configure(Config{Profile: "debug", Exporter: "none", ServiceName: "krill-test", ConsoleDebug: true}, testLogger(&buf))
	defer Shutdown(context.Background())

	span := StartSpan(nil, "", "", "agent.turn", "turn", 1)
	span.End(nil)
	if !bytes.Contains(buf.Bytes(), []byte("otel span end")) {
		t.Fatalf("expected textual console debug span log, got: %s", buf.String())
	}
}

//go:build !race

package telemetry

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

type noopHandler struct{}

func (noopHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (noopHandler) Handle(context.Context, slog.Record) error { return nil }
func (noopHandler) WithAttrs([]slog.Attr) slog.Handler        { return noopHandler{} }
func (noopHandler) WithGroup(string) slog.Handler             { return noopHandler{} }

func benchmarkLogger() *slog.Logger {
	return slog.New(noopHandler{})
}

func benchmarkWorkload(iter int) {
	acc := 0
	for i := 0; i < iter; i++ {
		for j := 0; j < 80000; j++ {
			acc += (i + j) % 7
		}
		span := StartSpan(nil, "trace-bench", "", "agent.turn")
		IncCounter(MetricMemoryOpsTotal, 1, map[string]string{"op": "append"})
		SetGauge(MetricActiveLoops, int64(i%17), nil)
		span.End(nil)
	}
	if acc == -1 {
		panic("unreachable")
	}
}

func measureWorkloadDuration(profile string, iter int) time.Duration {
	Configure(Config{Profile: profile, Exporter: "none", ServiceName: "krill-bench"}, benchmarkLogger())
	defer Shutdown(context.Background())
	start := time.Now()
	benchmarkWorkload(iter)
	return time.Since(start)
}

func TestProfileOverheadBudgets(t *testing.T) {
	const iterations = 1000
	base := measureWorkloadDuration("off", iterations)
	minimal := measureWorkloadDuration("minimal", iterations)
	standard := measureWorkloadDuration("standard", iterations)

	baseNs := float64(base.Nanoseconds())
	if baseNs <= 0 {
		t.Fatalf("invalid baseline duration: %s", base)
	}
	minimalOverhead := (float64(minimal.Nanoseconds()) - baseNs) / baseNs
	standardOverhead := (float64(standard.Nanoseconds()) - baseNs) / baseNs
	if minimalOverhead > 0.03 {
		t.Fatalf("minimal overhead %.2f%% exceeds 3%% budget", minimalOverhead*100)
	}
	if standardOverhead > 0.08 {
		t.Fatalf("standard overhead %.2f%% exceeds 8%% budget", standardOverhead*100)
	}
}

func BenchmarkObservabilityProfiles(b *testing.B) {
	for _, profile := range []string{"off", "minimal", "standard"} {
		b.Run(profile, func(b *testing.B) {
			Configure(Config{Profile: profile, Exporter: "none", ServiceName: "krill-bench"}, benchmarkLogger())
			defer Shutdown(context.Background())
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				span := StartSpan(nil, "trace-bench", "", "agent.turn")
				IncCounter(MetricMemoryOpsTotal, 1, nil)
				span.End(nil)
			}
		})
	}
}

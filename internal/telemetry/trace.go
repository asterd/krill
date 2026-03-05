package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Profile is the named telemetry overhead profile.
type Profile string

const (
	ProfileOff      Profile = "off"
	ProfileMinimal  Profile = "minimal"
	ProfileStandard Profile = "standard"
	ProfileDebug    Profile = "debug"
)

// Config configures telemetry exporters, sampling, and service identity.
type Config struct {
	Profile         string
	Exporter        string
	Endpoint        string
	ServiceName     string
	SampleRate      float64
	FlushIntervalMs int
	ConsoleDebug    bool
}

type runtimeState struct {
	cfg          Config
	profile      Profile
	log          *slog.Logger
	tracer       trace.Tracer
	meter        metric.Meter
	tp           *sdktrace.TracerProvider
	mp           *sdkmetric.MeterProvider
	shutdownOnce sync.Once
	knownMetrics *metricStore
	counters     sync.Map // map[string]metric.Int64Counter
	histos       sync.Map // map[string]metric.Int64Histogram
	gauges       sync.Map // map[string]gaugeState
	seq          atomic.Uint64
}

type gaugeState struct {
	inst metric.Int64UpDownCounter
	vals sync.Map // key -> int64
}

// Span is the lightweight tracing handle returned by StartSpan.
type Span struct {
	rt        *runtimeState
	log       *slog.Logger
	traceID   string
	spanID    string
	parentID  string
	name      string
	startedAt time.Time
	span      trace.Span
	attrs     []attribute.KeyValue
	enabled   bool
}

var current atomic.Pointer[runtimeState]

func defaultConfig() Config {
	return Config{
		Profile:         string(ProfileOff),
		Exporter:        "none",
		ServiceName:     "krill",
		SampleRate:      1.0,
		FlushIntervalMs: 5000,
		ConsoleDebug:    false,
	}
}

// Configure initializes telemetry for the current process.
func Configure(cfg Config, log *slog.Logger) {
	stopCurrent()
	cfg = normalizeConfig(cfg)
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	rt := &runtimeState{
		cfg:          cfg,
		profile:      Profile(cfg.Profile),
		log:          log,
		knownMetrics: newMetricStore(),
	}
	preRegisterCoreMetrics(rt.knownMetrics)

	res, err := resource.New(context.Background(), resource.WithAttributes(
		semconv.ServiceName(cfg.ServiceName),
	))
	if err != nil {
		log.Warn("otel resource init failed", "err", err)
		res = resource.Empty()
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))
	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	}

	mpOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	flush := time.Duration(cfg.FlushIntervalMs) * time.Millisecond
	if flush <= 0 {
		flush = 5 * time.Second
	}

	if cfg.Profile != string(ProfileOff) {
		switch cfg.Exporter {
		case "otlp_http":
			if cfg.Endpoint != "" {
				traceExp, traceErr := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpointURL(cfg.Endpoint), otlptracehttp.WithInsecure())
				if traceErr == nil {
					tpOpts = append(tpOpts, sdktrace.WithBatcher(traceExp))
				} else {
					log.Warn("otlp trace exporter init failed", "err", traceErr)
				}
				metricExp, metricErr := otlpmetrichttp.New(context.Background(), otlpmetrichttp.WithEndpointURL(cfg.Endpoint), otlpmetrichttp.WithInsecure())
				if metricErr == nil {
					reader := sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(flush))
					mpOpts = append(mpOpts, sdkmetric.WithReader(reader))
				} else {
					log.Warn("otlp metric exporter init failed", "err", metricErr)
				}
			}
		case "log":
			traceExp, traceErr := stdouttrace.New(stdouttrace.WithWriter(os.Stdout), stdouttrace.WithPrettyPrint())
			if traceErr == nil {
				tpOpts = append(tpOpts, sdktrace.WithBatcher(traceExp))
			}
			metricExp, metricErr := stdoutmetric.New(stdoutmetric.WithWriter(os.Stdout), stdoutmetric.WithPrettyPrint())
			if metricErr == nil {
				reader := sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(flush))
				mpOpts = append(mpOpts, sdkmetric.WithReader(reader))
			}
		}
	}

	rt.tp = sdktrace.NewTracerProvider(tpOpts...)
	rt.mp = sdkmetric.NewMeterProvider(mpOpts...)
	rt.tracer = rt.tp.Tracer("krill")
	rt.meter = rt.mp.Meter("krill")

	otel.SetTracerProvider(rt.tp)
	otel.SetMeterProvider(rt.mp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if cfg.Profile != string(ProfileOff) {
		if runErr := otelruntime.Start(otelruntime.WithMeterProvider(rt.mp)); runErr != nil {
			log.Warn("otel runtime metrics start failed", "err", runErr)
		}
	}

	current.Store(rt)
}

// Shutdown flushes and closes telemetry providers.
func Shutdown(ctx context.Context) {
	rt := current.Swap(nil)
	if rt == nil {
		return
	}
	rt.shutdownOnce.Do(func() {
		if rt.mp != nil {
			_ = rt.mp.Shutdown(ctx)
		}
		if rt.tp != nil {
			_ = rt.tp.Shutdown(ctx)
		}
	})
}

func stopCurrent() {
	Shutdown(context.Background())
}

func normalizeConfig(cfg Config) Config {
	def := defaultConfig()
	cfg.Profile = strings.ToLower(strings.TrimSpace(cfg.Profile))
	if cfg.Profile == "" {
		cfg.Profile = def.Profile
	}
	cfg.Exporter = strings.ToLower(strings.TrimSpace(cfg.Exporter))
	if cfg.Exporter == "" {
		switch cfg.Profile {
		case string(ProfileOff):
			cfg.Exporter = "none"
		default:
			cfg.Exporter = "otlp_http"
		}
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = def.ServiceName
	}
	if cfg.SampleRate <= 0 {
		switch cfg.Profile {
		case string(ProfileMinimal):
			cfg.SampleRate = 0.10
		case string(ProfileStandard):
			cfg.SampleRate = 0.35
		case string(ProfileDebug):
			cfg.SampleRate = 1.0
		default:
			cfg.SampleRate = 1.0
		}
	}
	if cfg.SampleRate > 1 {
		cfg.SampleRate = 1
	}
	if cfg.FlushIntervalMs <= 0 {
		cfg.FlushIntervalMs = def.FlushIntervalMs
	}
	return cfg
}

// NewTraceID returns a new trace identifier.
func NewTraceID() string {
	return randomHex(16)
}

// NewSpanID returns a new span identifier.
func NewSpanID() string {
	return randomHex(8)
}

// StartSpan starts a trace span and records optional structured attributes.
func StartSpan(log *slog.Logger, traceID, parentSpanID, name string, attrs ...any) *Span {
	rt := current.Load()
	if rt == nil || rt.profile == ProfileOff {
		return &Span{traceID: traceID, spanID: parentSpanID, name: name, enabled: false}
	}
	if rt.profile == ProfileMinimal && !isMinimalSpan(name) {
		return &Span{traceID: traceID, spanID: parentSpanID, name: name, enabled: false}
	}
	if log == nil {
		log = rt.log
	}
	if traceID == "" {
		traceID = NewTraceID()
	}

	ctx := context.Background()
	if pctx := remoteParent(traceID, parentSpanID); pctx.IsValid() {
		ctx = trace.ContextWithRemoteSpanContext(ctx, pctx)
	}

	otAttrs := attrsToOTel(attrs)
	ctx, otSpan := rt.tracer.Start(ctx, name, trace.WithAttributes(otAttrs...))
	_ = ctx

	sc := otSpan.SpanContext()
	span := &Span{
		rt:        rt,
		log:       log,
		traceID:   sc.TraceID().String(),
		spanID:    sc.SpanID().String(),
		parentID:  parentSpanID,
		name:      name,
		startedAt: time.Now(),
		span:      otSpan,
		attrs:     otAttrs,
		enabled:   true,
	}
	if span.traceID == "00000000000000000000000000000000" {
		span.traceID = traceID
	}
	if span.spanID == "0000000000000000" {
		span.spanID = NewSpanID()
	}
	return span
}

func (s *Span) End(err error, attrs ...any) {
	if s == nil || !s.enabled {
		return
	}
	if s.span != nil {
		if len(attrs) > 0 {
			s.span.SetAttributes(attrsToOTel(attrs)...)
		}
		if err != nil {
			s.span.RecordError(err)
			s.span.SetStatus(codes.Error, err.Error())
		} else {
			s.span.SetStatus(codes.Ok, "")
		}
		s.span.End()
	}
	if s.rt != nil && s.rt.cfg.ConsoleDebug && s.log != nil {
		s.log.Info("otel span end",
			"trace_id", s.traceID,
			"span_id", s.spanID,
			"parent_span_id", s.parentID,
			"span_name", s.name,
			"duration_ms", time.Since(s.startedAt).Milliseconds(),
			"status", statusOf(err),
		)
	}
}

func statusOf(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

func (s *Span) TraceID() string {
	if s == nil {
		return ""
	}
	return s.traceID
}

func (s *Span) SpanID() string {
	if s == nil {
		return ""
	}
	return s.spanID
}

// SnapshotMetrics returns the in-memory metric snapshot used by tests and debug paths.
func SnapshotMetrics() []metricSample {
	rt := current.Load()
	if rt == nil {
		return nil
	}
	return rt.knownMetrics.snapshot()
}

// ResetForTests resets global telemetry state between tests.
func ResetForTests() {
	Shutdown(context.Background())
	Configure(defaultConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func currentProfile() Profile {
	rt := current.Load()
	if rt == nil {
		return ProfileOff
	}
	return rt.profile
}

func enabled() bool {
	return currentProfile() != ProfileOff
}

func metricEnabled(name string) bool {
	rt := current.Load()
	if rt == nil || rt.profile == ProfileOff {
		return false
	}
	return rt.knownMetrics.isKnown(name)
}

func attrsToOTel(attrs []any) []attribute.KeyValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]attribute.KeyValue, 0, len(attrs)/2)
	for i := 0; i+1 < len(attrs); i += 2 {
		k, ok := attrs[i].(string)
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		out = append(out, attribute.String(k, fmt.Sprint(attrs[i+1])))
	}
	return out
}

func labelsToOTel(labels map[string]string) []attribute.KeyValue {
	if len(labels) == 0 {
		return nil
	}
	out := make([]attribute.KeyValue, 0, len(labels))
	for k, v := range labels {
		if strings.TrimSpace(k) == "" {
			continue
		}
		out = append(out, attribute.String(k, v))
	}
	return out
}

func remoteParent(traceID, spanID string) trace.SpanContext {
	tid, tErr := trace.TraceIDFromHex(traceID)
	sid, sErr := trace.SpanIDFromHex(spanID)
	if tErr != nil || sErr != nil {
		return trace.SpanContext{}
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
}

func isMinimalSpan(name string) bool {
	switch name {
	case "ingress.receive", "ingress.validate", "ingress.publish", "bus.consume", "orchestrator.route", "agent.react", "agent.turn", "llm.call":
		return true
	default:
		return false
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()+int64(n))
	}
	return hex.EncodeToString(b)
}

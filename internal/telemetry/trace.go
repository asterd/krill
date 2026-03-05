// Package telemetry provides ultra-light trace/span logging primitives.
// It is intentionally minimal and log-first, so it can later feed OTEL exporters.
package telemetry

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"
)

// NewTraceID returns a 16-byte hex trace id (32 chars).
func NewTraceID() string {
	return randomHex(16)
}

// NewSpanID returns an 8-byte hex span id (16 chars).
func NewSpanID() string {
	return randomHex(8)
}

// Span is a lightweight log span.
type Span struct {
	log       *slog.Logger
	traceID   string
	spanID    string
	parentID  string
	name      string
	startedAt time.Time
}

// StartSpan emits a span start log and returns a span handle.
func StartSpan(log *slog.Logger, traceID, parentSpanID, name string, attrs ...any) *Span {
	if traceID == "" {
		traceID = NewTraceID()
	}
	span := &Span{
		log:       log,
		traceID:   traceID,
		spanID:    NewSpanID(),
		parentID:  parentSpanID,
		name:      name,
		startedAt: time.Now(),
	}

	fields := []any{
		"trace_id", span.traceID,
		"span_id", span.spanID,
		"parent_span_id", span.parentID,
		"span_name", span.name,
	}
	fields = append(fields, attrs...)
	span.log.Info("span start", fields...)
	return span
}

// End emits a span end log including duration.
func (s *Span) End(err error, attrs ...any) {
	if s == nil || s.log == nil {
		return
	}
	fields := []any{
		"trace_id", s.traceID,
		"span_id", s.spanID,
		"parent_span_id", s.parentID,
		"span_name", s.name,
		"duration_ms", time.Since(s.startedAt).Milliseconds(),
	}
	if err != nil {
		fields = append(fields, "status", "error", "err", err.Error())
	} else {
		fields = append(fields, "status", "ok")
	}
	fields = append(fields, attrs...)
	s.log.Info("span end", fields...)
}

// TraceID returns the span trace id.
func (s *Span) TraceID() string {
	if s == nil {
		return ""
	}
	return s.traceID
}

// SpanID returns the current span id.
func (s *Span) SpanID() string {
	if s == nil {
		return ""
	}
	return s.spanID
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		now := time.Now().UnixNano()
		return hex.EncodeToString([]byte(time.Unix(0, now).Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b)
}

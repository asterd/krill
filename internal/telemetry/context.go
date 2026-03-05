package telemetry

import "context"

type traceContextKey string

const (
	keyTraceID   traceContextKey = "trace_id"
	keySpanID    traceContextKey = "span_id"
	keyRequestID traceContextKey = "request_id"
)

// WithTrace stores trace metadata in context.
func WithTrace(ctx context.Context, traceID, spanID, requestID string) context.Context {
	ctx = context.WithValue(ctx, keyTraceID, traceID)
	ctx = context.WithValue(ctx, keySpanID, spanID)
	ctx = context.WithValue(ctx, keyRequestID, requestID)
	return ctx
}

// TraceFromContext returns trace metadata if present.
func TraceFromContext(ctx context.Context) (traceID, spanID, requestID string) {
	if v, ok := ctx.Value(keyTraceID).(string); ok {
		traceID = v
	}
	if v, ok := ctx.Value(keySpanID).(string); ok {
		spanID = v
	}
	if v, ok := ctx.Value(keyRequestID).(string); ok {
		requestID = v
	}
	return traceID, spanID, requestID
}

package ingress

import (
	"context"
	"fmt"

	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/schema"
	"github.com/krill/krill/internal/telemetry"
)

type Normalizer struct {
	opts schema.NormalizeOptions
}

func NewNormalizer(strictV2Validation bool) *Normalizer {
	return &Normalizer{
		opts: schema.NormalizeOptions{
			StrictV2Validation: strictV2Validation,
		},
	}
}

func (n *Normalizer) NormalizeInbound(env *bus.Envelope) (*bus.Envelope, error) {
	traceID := ""
	parentSpan := ""
	if env != nil && env.Meta != nil {
		traceID = env.Meta["trace_id"]
		parentSpan = env.Meta["ingress_span"]
	}
	recvSpan := telemetry.StartSpan(nil, traceID, parentSpan, "ingress.receive")
	defer recvSpan.End(nil)

	v2 := schema.DefaultV2(schema.BusToV2(env))
	validateSpan := telemetry.StartSpan(nil, recvSpan.TraceID(), recvSpan.SpanID(), "ingress.validate")
	if err := schema.ValidateV2(v2, n.opts.StrictV2Validation); err != nil {
		validateSpan.End(err)
		return nil, fmt.Errorf("validate envelope v2: %w", err)
	}
	validateSpan.End(nil)
	out := schema.V2ToBus(v2)
	if out.Meta == nil {
		out.Meta = map[string]string{}
	}
	if out.Meta["trace_id"] == "" {
		out.Meta["trace_id"] = recvSpan.TraceID()
	}
	out.Meta["ingress_span"] = recvSpan.SpanID()
	return out, nil
}

func (n *Normalizer) PublishInbound(ctx context.Context, b bus.Bus, env *bus.Envelope) error {
	normalized, err := n.NormalizeInbound(env)
	if err != nil {
		return err
	}
	publishSpan := telemetry.StartSpan(nil, normalized.Meta["trace_id"], normalized.Meta["ingress_span"], "ingress.publish")
	defer publishSpan.End(nil)
	return b.Publish(ctx, bus.InboundKey, normalized)
}

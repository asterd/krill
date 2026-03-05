package ingress

import (
	"context"
	"fmt"

	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/schema"
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
	v2 := schema.DefaultV2(schema.BusToV2(env))
	if err := schema.ValidateV2(v2, n.opts.StrictV2Validation); err != nil {
		return nil, fmt.Errorf("validate envelope v2: %w", err)
	}
	return schema.V2ToBus(v2), nil
}

func (n *Normalizer) PublishInbound(ctx context.Context, b bus.Bus, env *bus.Envelope) error {
	normalized, err := n.NormalizeInbound(env)
	if err != nil {
		return err
	}
	return b.Publish(ctx, bus.InboundKey, normalized)
}

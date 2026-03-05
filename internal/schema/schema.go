package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/krill/krill/internal/bus"
)

const VersionV2 = "v2"

var ErrInvalidSchema = errors.New("invalid schema")

type EnvelopeV2 struct {
	SchemaVersion  string            `json:"schema_version"`
	ID             string            `json:"id"`
	ClientID       string            `json:"client_id"`
	ThreadID       string            `json:"thread_id"`
	Tenant         string            `json:"tenant"`
	WorkflowID     string            `json:"workflow_id"`
	Hop            int               `json:"hop"`
	SourceProtocol string            `json:"source_protocol"`
	Role           string            `json:"role"`
	Text           string            `json:"text"`
	Meta           map[string]string `json:"meta,omitempty"`
	Capabilities   []string          `json:"capabilities,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
}

type NormalizeOptions struct {
	StrictV2Validation bool
}

func NormalizeJSON(data []byte, opts NormalizeOptions) (EnvelopeV2, error) {
	var v2 EnvelopeV2
	if err := json.Unmarshal(data, &v2); err != nil {
		return EnvelopeV2{}, fmt.Errorf("%w: decode v2 envelope: %v", ErrInvalidSchema, err)
	}
	if sv := strings.TrimSpace(strings.ToLower(v2.SchemaVersion)); sv != "" && sv != VersionV2 {
		return EnvelopeV2{}, fmt.Errorf("%w: unsupported schema_version %q", ErrInvalidSchema, v2.SchemaVersion)
	}
	v2 = DefaultV2(v2)
	if err := ValidateV2(v2, opts.StrictV2Validation); err != nil {
		return EnvelopeV2{}, err
	}
	return v2, nil
}

func DefaultV2(v2 EnvelopeV2) EnvelopeV2 {
	if strings.TrimSpace(v2.SchemaVersion) == "" {
		v2.SchemaVersion = VersionV2
	}
	if strings.TrimSpace(v2.ID) == "" {
		v2.ID = uuid.NewString()
	}
	if strings.TrimSpace(v2.ThreadID) == "" {
		v2.ThreadID = v2.ClientID
	}
	if strings.TrimSpace(v2.Tenant) == "" {
		v2.Tenant = "default"
	}
	if strings.TrimSpace(v2.WorkflowID) == "" {
		v2.WorkflowID = "default"
	}
	if v2.Meta == nil {
		v2.Meta = map[string]string{}
	}
	if v2.Capabilities == nil {
		v2.Capabilities = []string{}
	}
	if v2.CreatedAt.IsZero() {
		v2.CreatedAt = time.Now().UTC()
	}
	return v2
}

func ValidateV2(v2 EnvelopeV2, strict bool) error {
	if strict && strings.TrimSpace(strings.ToLower(v2.SchemaVersion)) != VersionV2 {
		return fmt.Errorf("%w: schema_version must be %q in strict mode", ErrInvalidSchema, VersionV2)
	}
	if strings.TrimSpace(v2.ID) == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidSchema)
	}
	if strings.TrimSpace(v2.ClientID) == "" {
		return fmt.Errorf("%w: client_id is required", ErrInvalidSchema)
	}
	if strings.TrimSpace(v2.ThreadID) == "" {
		return fmt.Errorf("%w: thread_id is required", ErrInvalidSchema)
	}
	if strings.TrimSpace(v2.SourceProtocol) == "" {
		return fmt.Errorf("%w: source_protocol is required", ErrInvalidSchema)
	}
	if strings.TrimSpace(v2.Role) == "" {
		return fmt.Errorf("%w: role is required", ErrInvalidSchema)
	}
	if strings.TrimSpace(v2.Tenant) == "" {
		return fmt.Errorf("%w: tenant is required", ErrInvalidSchema)
	}
	if strings.TrimSpace(v2.WorkflowID) == "" {
		return fmt.Errorf("%w: workflow_id is required", ErrInvalidSchema)
	}
	if v2.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at is required", ErrInvalidSchema)
	}
	return nil
}

func BusToV2(env *bus.Envelope) EnvelopeV2 {
	if env == nil {
		return EnvelopeV2{}
	}
	return EnvelopeV2{
		SchemaVersion:  VersionV2,
		ID:             env.ID,
		ClientID:       env.ClientID,
		ThreadID:       env.ThreadID,
		SourceProtocol: env.SourceProtocol,
		Role:           string(env.Role),
		Text:           env.Text,
		Meta:           cloneMeta(env.Meta),
		CreatedAt:      env.CreatedAt,
	}
}

func V2ToBus(v2 EnvelopeV2) *bus.Envelope {
	v2 = DefaultV2(v2)
	meta := cloneMeta(v2.Meta)
	meta["schema_version"] = v2.SchemaVersion
	meta["tenant"] = v2.Tenant
	meta["workflow_id"] = v2.WorkflowID
	meta["hop"] = fmt.Sprintf("%d", v2.Hop)
	if len(v2.Capabilities) > 0 {
		meta["capabilities"] = strings.Join(v2.Capabilities, ",")
	}
	return &bus.Envelope{
		ID:             v2.ID,
		ClientID:       v2.ClientID,
		ThreadID:       v2.ThreadID,
		Role:           bus.Role(v2.Role),
		Text:           v2.Text,
		SourceProtocol: v2.SourceProtocol,
		Meta:           meta,
		CreatedAt:      v2.CreatedAt,
	}
}

func cloneMeta(meta map[string]string) map[string]string {
	if len(meta) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	return out
}

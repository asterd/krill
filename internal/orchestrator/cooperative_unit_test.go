package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
)

func TestWorkflowForSelection(t *testing.T) {
	orch := &Orch{cfg: &config.Root{Workflows: []config.WorkflowConfig{{ID: "wf-1", OrchestrationMode: "cooperative"}}}}
	wf, ok := orch.workflowFor(&bus.Envelope{Meta: map[string]string{"workflow_id": "wf-1"}})
	if !ok || wf.ID != "wf-1" {
		t.Fatalf("expected wf-1, got ok=%v wf=%+v", ok, wf)
	}
	if got, ok := orch.workflowFor(&bus.Envelope{Meta: map[string]string{"workflow_id": "missing"}}); ok || got.ID != "" {
		t.Fatalf("expected missing workflow, got ok=%v wf=%+v", ok, got)
	}
}

func TestFindHelpersAndPublishFailure(t *testing.T) {
	b := bus.NewLocal(4)
	orch := &Orch{
		cfg: &config.Root{
			Agents: []config.AgentConfig{{Name: "a1", LLM: "mock"}},
			OrgSchemas: []config.OrgSchemaConfig{{
				SchemaID: "schema-1",
				Roles: []config.OrgRoleConfig{
					{Name: "router", Kind: "router", Agent: "a1"},
					{Name: "specialist", Kind: "specialist", Agent: "a1"},
					{Name: "synth", Kind: "synthesizer", Agent: "a1"},
				},
			}},
		},
		b:   b,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if _, err := orch.findAgentConfig("missing"); err == nil {
		t.Fatal("expected missing agent error")
	}
	if _, err := orch.findOrgSchema("missing"); err == nil {
		t.Fatal("expected missing org schema error")
	}

	replyCh := b.Subscribe(bus.ReplyKey("http"))
	env := &bus.Envelope{ID: "req-1", ClientID: "c1", ThreadID: "t1", SourceProtocol: "http", Meta: map[string]string{"trace_id": "t"}}
	orch.publishCooperativeFailure(context.Background(), env, config.WorkflowConfig{ID: "wf-1"}, context.DeadlineExceeded)
	select {
	case out := <-replyCh:
		if out.Meta["status"] != "blocked" {
			t.Fatalf("expected blocked status, got %+v", out.Meta)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting failure reply")
	}
}

func TestCompileOrgSchemaRejectsInvalid(t *testing.T) {
	_, err := compileOrgSchema(config.OrgSchemaConfig{SchemaID: "bad"})
	if err == nil {
		t.Fatal("expected compile error")
	}
}

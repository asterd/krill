package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/agent"
	"github.com/krill/krill/internal/bus"
)

func TestSelectAgentPriorityAndRoundRobin(t *testing.T) {
	cfg := &config.Root{
		LLM: config.LLMPool{Default: "def"},
		Agents: []config.AgentConfig{
			{Name: "proto-http", MatchProtocol: "http", LLM: "def"},
			{Name: "rr-a", LLM: "def"},
			{Name: "rr-b", LLM: "def"},
		},
	}
	orch := &Orch{cfg: cfg}

	got := orch.selectAgent(&bus.Envelope{
		SourceProtocol: "http",
		Meta:           map[string]string{},
	}, "")
	if got.Name != "proto-http" {
		t.Fatalf("expected protocol match agent, got %s", got.Name)
	}

	got1 := orch.selectAgent(&bus.Envelope{
		SourceProtocol: "telegram",
		Meta:           map[string]string{},
	}, "")
	got2 := orch.selectAgent(&bus.Envelope{
		SourceProtocol: "telegram",
		Meta:           map[string]string{},
	}, "")
	if got1.Name == got2.Name {
		t.Fatalf("expected round-robin distribution, got %s twice", got1.Name)
	}
}

func TestDefaultAgentAndString(t *testing.T) {
	agent := defaultAgent("mock")
	if agent.Name == "" || agent.LLM != "mock" {
		t.Fatalf("unexpected default agent: %+v", agent)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch, err := New(&config.Root{
		Core: config.CoreConfig{
			MaxClients: 2,
		},
	}, bus.NewLocal(2), nil, nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	if orch.String() == "" {
		t.Fatal("String should not be empty")
	}
}

func TestDispatchSkipsNonUserAndStopsOnClosedChannel(t *testing.T) {
	orch := &Orch{
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		loops: map[string]*agent.Loop{},
		sem:   make(chan struct{}, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan *bus.Envelope, 1)
	ch <- &bus.Envelope{Role: bus.RoleAssistant}
	close(ch)
	orch.dispatch(ctx, ch)
}

func TestRouteMaxClientsReachedBranch(t *testing.T) {
	orch := &Orch{
		cfg: &config.Root{
			LLM: config.LLMPool{Default: "d"},
		},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		loops: map[string]*agent.Loop{},
		sem:   make(chan struct{}, 1),
	}
	orch.sem <- struct{}{} // saturate semaphore
	env := &bus.Envelope{
		ClientID:       "c1",
		SourceProtocol: "http",
		Role:           bus.RoleUser,
		Meta:           map[string]string{"trace_id": "t"},
		CreatedAt:      time.Now().UTC(),
	}
	orch.route(context.Background(), env)
	if orch.ActiveCount() != 0 {
		t.Fatalf("expected no loops started when max clients reached")
	}
}

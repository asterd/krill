package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/agent"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
	"github.com/krill/krill/internal/session"
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
	}, bus.NewLocal(2), nil, nil, nil, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	if orch.String() == "" {
		t.Fatal("String should not be empty")
	}
}

func TestSelectAgentScheduledTargetOverride(t *testing.T) {
	cfg := &config.Root{
		LLM: config.LLMPool{Default: "def"},
		Agents: []config.AgentConfig{
			{Name: "default-a", LLM: "def"},
			{Name: "scheduled-b", LLM: "def"},
		},
	}
	orch := &Orch{cfg: cfg}
	got := orch.selectAgent(&bus.Envelope{
		SourceProtocol: "scheduler",
		Meta:           map[string]string{"target_agent": "scheduled-b"},
	}, "")
	if got.Name != "scheduled-b" {
		t.Fatalf("expected scheduled target agent, got %s", got.Name)
	}
}

func TestRestoreSessionContextHydratesMemoryAndOpensPersistent(t *testing.T) {
	svc, err := session.NewService(config.SessionConfig{
		Enabled:                  true,
		Path:                     filepath.Join(t.TempDir(), "sessions.json"),
		RetentionMaxMessages:     10,
		SummarizationThreshold:   0,
		SummarizationKeepRecent:  0,
		DefaultMergeConflictMode: "last-write-wins",
	})
	if err != nil {
		t.Fatal(err)
	}
	baseMem := memory.NewRAM()
	mem := session.WrapMemoryStore(baseMem, svc)
	orch := &Orch{
		mem:      mem,
		sessions: svc,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if _, err := svc.Open(session.OpenRequest{ClientID: "c1", ThreadID: "t1", Mode: session.ModePersistent}, session.Provenance{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordMessage("c1", "t1", llm.Message{Role: "user", Content: "persisted"}, session.Provenance{}); err != nil {
		t.Fatal(err)
	}
	orch.restoreSessionContext(&bus.Envelope{
		ClientID:  "c1",
		ThreadID:  "t1",
		Meta:      map[string]string{"session_mode": "persistent", "tenant": "tenant-a"},
		CreatedAt: time.Now().UTC(),
	})
	msgs := baseMem.Get("c1", "t1", 10)
	if len(msgs) != 1 || msgs[0].Content != "persisted" {
		t.Fatalf("expected session hydration into memory, got %+v", msgs)
	}

	orch.restoreSessionContext(&bus.Envelope{
		ClientID:  "c2",
		ThreadID:  "t2",
		Meta:      map[string]string{"session_mode": "persistent", "tenant": "tenant-b"},
		CreatedAt: time.Now().UTC(),
	})
	if _, ok, err := svc.ResumeByThread("c2", "t2", session.Provenance{}); err != nil || !ok {
		t.Fatalf("expected persistent session auto-open, ok=%v err=%v", ok, err)
	}
}

func TestRestoreSessionContextSkipsEphemeralOpenAndHelpers(t *testing.T) {
	svc, err := session.NewService(config.SessionConfig{
		Enabled:                  true,
		Path:                     filepath.Join(t.TempDir(), "sessions.json"),
		RetentionMaxMessages:     10,
		SummarizationThreshold:   0,
		SummarizationKeepRecent:  0,
		DefaultMergeConflictMode: "last-write-wins",
	})
	if err != nil {
		t.Fatal(err)
	}
	orch := &Orch{
		mem:      memory.NewRAM(),
		sessions: svc,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	orch.restoreSessionContext(&bus.Envelope{
		ClientID: "e1",
		ThreadID: "e1",
		Meta:     map[string]string{"session_mode": "ephemeral"},
	})
	if _, ok, err := svc.ResumeByThread("e1", "e1", session.Provenance{}); err != nil || ok {
		t.Fatalf("expected ephemeral flow not to auto-open, ok=%v err=%v", ok, err)
	}
	if !shouldOpenPersistentSession(&bus.Envelope{}) {
		t.Fatal("expected nil/empty meta to default to persistent")
	}
	if shouldOpenPersistentSession(&bus.Envelope{Meta: map[string]string{"session_mode": "ephemeral"}}) {
		t.Fatal("expected ephemeral session mode to disable auto-open")
	}
	if got := defaultSessionMode(""); got != string(session.ModePersistent) {
		t.Fatalf("unexpected default session mode %q", got)
	}
	if got := defaultSessionMode("ephemeral"); got != "ephemeral" {
		t.Fatalf("unexpected explicit session mode %q", got)
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

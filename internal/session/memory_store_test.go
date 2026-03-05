package session

import (
	"path/filepath"
	"testing"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
)

func TestWrapMemoryStoreAndHydrate(t *testing.T) {
	svc, err := NewService(config.SessionConfig{
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
	if _, err := svc.Open(OpenRequest{ClientID: "c1", ThreadID: "t1", Mode: ModePersistent}, Provenance{}); err != nil {
		t.Fatal(err)
	}

	base := memory.NewRAM()
	wrapped := WrapMemoryStore(base, svc)
	if wrapped == nil {
		t.Fatal("expected wrapped memory store")
	}
	wrapped.Append("c1", "t1", llm.Message{Role: "user", Content: "hello"})
	if got := wrapped.Get("c1", "t1", 10); len(got) != 1 {
		t.Fatalf("expected memory get to delegate, got %+v", got)
	}
	msgs, ok, err := svc.RestoreMessagesByThread("c1", "t1")
	if err != nil || !ok {
		t.Fatalf("restore by thread failed ok=%v err=%v", ok, err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected session persistence from wrapped append, got %+v", msgs)
	}

	hydrator, ok := wrapped.(Hydrator)
	if !ok {
		t.Fatal("expected wrapped store to implement Hydrator")
	}
	hydrator.Hydrate("c1", "t1", []llm.Message{{Role: "assistant", Content: "restored"}})
	if got := wrapped.Get("c1", "t1", 10); len(got) != 2 {
		t.Fatalf("expected hydrated message in memory, got %+v", got)
	}
	msgs, ok, err = svc.RestoreMessagesByThread("c1", "t1")
	if err != nil || !ok {
		t.Fatalf("restore by thread failed ok=%v err=%v", ok, err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected hydrate not to duplicate session history, got %+v", msgs)
	}
	wrapped.Clear("c1", "t1")
	if got := wrapped.Get("c1", "t1", 10); len(got) != 0 {
		t.Fatalf("expected delegated clear, got %+v", got)
	}
}

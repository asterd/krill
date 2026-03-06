package session

import (
	"path/filepath"
	"testing"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
)

type closableStore struct {
	memory.Store
	closed bool
}

func (c *closableStore) Close() error {
	c.closed = true
	return nil
}

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
	if err := wrapped.Append("c1", "t1", llm.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Flush(); err != nil {
		t.Fatal(err)
	}
	gotMsgs, err := wrapped.Get("c1", "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(gotMsgs); got != 1 {
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
	gotMsgs, err = wrapped.Get("c1", "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(gotMsgs); got != 2 {
		t.Fatalf("expected hydrated message in memory, got %+v", got)
	}
	msgs, ok, err = svc.RestoreMessagesByThread("c1", "t1")
	if err != nil || !ok {
		t.Fatalf("restore by thread failed ok=%v err=%v", ok, err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected hydrate not to duplicate session history, got %+v", msgs)
	}
	if err := wrapped.Clear("c1", "t1"); err != nil {
		t.Fatal(err)
	}
	gotMsgs, err = wrapped.Get("c1", "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(gotMsgs); got != 0 {
		t.Fatalf("expected delegated clear, got %+v", got)
	}
}

func TestMemoryStore_AppendBatchSnapshotRestoreTrimClose(t *testing.T) {
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
	if _, err := svc.Open(OpenRequest{ClientID: "c2", ThreadID: "t2", Mode: ModePersistent}, Provenance{}); err != nil {
		t.Fatal(err)
	}
	base := &closableStore{Store: memory.NewRAM()}
	wrapped := WrapMemoryStore(base, svc)
	if err := wrapped.AppendBatch("c2", "t2", []llm.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Flush(); err != nil {
		t.Fatal(err)
	}
	snap, err := wrapped.Snapshot("c2", "t2")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 2 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if err := wrapped.Trim("c2", "t2", 1); err != nil {
		t.Fatal(err)
	}
	got, err := wrapped.Get("c2", "t2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("unexpected trimmed messages: %+v", got)
	}
	if err := wrapped.Restore("c2", "t2", snap); err != nil {
		t.Fatal(err)
	}
	got, err = wrapped.Get("c2", "t2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("unexpected restored messages: %+v", got)
	}
	closer, ok := wrapped.(interface{ Close() error })
	if !ok {
		t.Fatal("expected wrapped store to implement Close")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if !base.closed {
		t.Fatal("expected base closer to be called")
	}
}

func TestWrapMemoryStore_NilInputs(t *testing.T) {
	if got := WrapMemoryStore(nil, nil); got != nil {
		t.Fatalf("expected nil passthrough, got %#v", got)
	}
	base := memory.NewRAM()
	if got := WrapMemoryStore(base, nil); got != base {
		t.Fatal("expected base passthrough when service is nil")
	}
}

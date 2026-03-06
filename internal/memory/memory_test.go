package memory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/krill/krill/internal/llm"
)

func TestNewStore_BackendSelection(t *testing.T) {
	if _, err := NewStore("ram", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore("sqlite", filepath.Join(t.TempDir(), "memory.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore("unknown", ""); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestFileStore_PersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	store, err := NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append("c1", "t1", llm.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append("c1", "t1", llm.Message{Role: "assistant", Content: "world"}); err != nil {
		t.Fatal(err)
	}

	store2, err := NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := store2.Get("c1", "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 msgs, got %d", len(msgs))
	}
	if msgs[0].Content != "hello" || msgs[1].Content != "world" {
		t.Fatalf("unexpected persisted data: %+v", msgs)
	}

	if err := store2.Clear("c1", "t1"); err != nil {
		t.Fatal(err)
	}
	store3, err := NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err = store3.Get("c1", "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(msgs); got != 0 {
		t.Fatalf("expected empty after clear, got %d", got)
	}
}

func TestRAMStore_BasicOperations(t *testing.T) {
	store := NewRAM()
	_ = store.Append("c1", "t1", llm.Message{Role: "user", Content: "one"})
	_ = store.Append("c1", "t1", llm.Message{Role: "assistant", Content: "two"})
	_ = store.Append("c2", "t1", llm.Message{Role: "user", Content: "other"})

	msgs, err := store.Get("c1", "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(msgs); got != 2 {
		t.Fatalf("expected 2 messages, got %d", got)
	}
	msgs, err = store.Get("c1", "t1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(msgs); got != 1 {
		t.Fatalf("expected windowed result 1, got %d", got)
	}
	_ = store.Clear("c1", "t1")
	msgs, err = store.Get("c1", "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(msgs); got != 0 {
		t.Fatalf("expected cleared thread, got %d", got)
	}
	msgs, err = store.Get("c2", "t1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(msgs); got != 1 {
		t.Fatalf("expected isolated thread to remain, got %d", got)
	}
}

func TestNewStore_FileBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	store, err := NewStore("file", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append("c", "t", llm.Message{Role: "user", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	msgs, err := store.Get("c", "t", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(msgs); got != 1 {
		t.Fatalf("expected 1 msg, got %d", got)
	}
}

func TestSQLiteStore_PersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	store, err := NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Append("c1", "t1", llm.Message{Role: "user", Content: "hello"})
	_ = store.Append("c1", "t1", llm.Message{Role: "assistant", Content: "world"})

	store2, err := NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := store2.Get("c1", "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 msgs, got %d", len(msgs))
	}
	if msgs[0].Content != "hello" || msgs[1].Content != "world" {
		t.Fatalf("unexpected persisted sqlite data: %+v", msgs)
	}

	_ = store2.Clear("c1", "t1")
	store3, err := NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err = store3.Get("c1", "t1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(msgs); got != 0 {
		t.Fatalf("expected empty after clear, got %d", got)
	}
}

func TestNewFile_LoadCorrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFile(path); err == nil {
		t.Fatal("expected decode error from corrupted file")
	}
}

func TestFileStore_PersistErrorPath(t *testing.T) {
	store, err := NewFile(filepath.Join(t.TempDir(), "ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	fs := store.(*fileStore)
	fs.mu.Lock()
	fs.path = "/dev/null/memory.json"
	fs.mu.Unlock()
	_ = store.Append("c", "t", llm.Message{Role: "user", Content: "x"})
}

func TestRAMStore_SnapshotRestoreTrim(t *testing.T) {
	store := NewRAM()
	_ = store.AppendBatch("c", "t", []llm.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
		{Role: "tool", Content: "three"},
	})
	snap, err := store.Snapshot("c", "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 3 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if err := store.Trim("c", "t", 2); err != nil {
		t.Fatal(err)
	}
	msgs, err := store.Get("c", "t", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Content != "two" {
		t.Fatalf("unexpected trimmed messages: %+v", msgs)
	}
	if err := store.Restore("c", "t", snap); err != nil {
		t.Fatal(err)
	}
	msgs, err = store.Get("c", "t", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("unexpected restored messages: %+v", msgs)
	}
	if err := store.Trim("c", "t", -1); err == nil {
		t.Fatal("expected trim validation error")
	}
}

func TestFileAndSQLiteStore_SnapshotRestoreTrim(t *testing.T) {
	cases := []struct {
		name string
		new  func(string) (Store, error)
		path string
	}{
		{name: "file", new: NewFile, path: filepath.Join(t.TempDir(), "memory.json")},
		{name: "sqlite", new: NewSQLite, path: filepath.Join(t.TempDir(), "memory.db")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := tc.new(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			_ = store.AppendBatch("c", "t", []llm.Message{
				{Role: "user", Content: "one"},
				{Role: "assistant", Content: "two"},
				{Role: "tool", Content: "three"},
			})
			snap, err := store.Snapshot("c", "t")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Trim("c", "t", 1); err != nil {
				t.Fatal(err)
			}
			msgs, err := store.Get("c", "t", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 || msgs[0].Content != "three" {
				t.Fatalf("unexpected trimmed messages: %+v", msgs)
			}
			if err := store.Restore("c", "t", snap); err != nil {
				t.Fatal(err)
			}
			msgs, err = store.Get("c", "t", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 3 {
				t.Fatalf("unexpected restored messages: %+v", msgs)
			}
			if err := store.Trim("c", "t", -1); err == nil {
				t.Fatal("expected trim validation error")
			}
			if closer, ok := store.(interface{ Close() error }); ok {
				if err := closer.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

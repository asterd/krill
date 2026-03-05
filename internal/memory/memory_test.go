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
	store.Append("c1", "t1", llm.Message{Role: "user", Content: "hello"})
	store.Append("c1", "t1", llm.Message{Role: "assistant", Content: "world"})

	store2, err := NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	msgs := store2.Get("c1", "t1", 10)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 msgs, got %d", len(msgs))
	}
	if msgs[0].Content != "hello" || msgs[1].Content != "world" {
		t.Fatalf("unexpected persisted data: %+v", msgs)
	}

	store2.Clear("c1", "t1")
	store3, err := NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(store3.Get("c1", "t1", 10)); got != 0 {
		t.Fatalf("expected empty after clear, got %d", got)
	}
}

func TestRAMStore_BasicOperations(t *testing.T) {
	store := NewRAM()
	store.Append("c1", "t1", llm.Message{Role: "user", Content: "one"})
	store.Append("c1", "t1", llm.Message{Role: "assistant", Content: "two"})
	store.Append("c2", "t1", llm.Message{Role: "user", Content: "other"})

	if got := len(store.Get("c1", "t1", 100)); got != 2 {
		t.Fatalf("expected 2 messages, got %d", got)
	}
	if got := len(store.Get("c1", "t1", 1)); got != 1 {
		t.Fatalf("expected windowed result 1, got %d", got)
	}
	store.Clear("c1", "t1")
	if got := len(store.Get("c1", "t1", 100)); got != 0 {
		t.Fatalf("expected cleared thread, got %d", got)
	}
	if got := len(store.Get("c2", "t1", 100)); got != 1 {
		t.Fatalf("expected isolated thread to remain, got %d", got)
	}
}

func TestNewStore_FileBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	store, err := NewStore("file", path)
	if err != nil {
		t.Fatal(err)
	}
	store.Append("c", "t", llm.Message{Role: "user", Content: "x"})
	if got := len(store.Get("c", "t", 10)); got != 1 {
		t.Fatalf("expected 1 msg, got %d", got)
	}
}

func TestSQLiteStore_PersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	store, err := NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Append("c1", "t1", llm.Message{Role: "user", Content: "hello"})
	store.Append("c1", "t1", llm.Message{Role: "assistant", Content: "world"})

	store2, err := NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	msgs := store2.Get("c1", "t1", 10)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 msgs, got %d", len(msgs))
	}
	if msgs[0].Content != "hello" || msgs[1].Content != "world" {
		t.Fatalf("unexpected persisted sqlite data: %+v", msgs)
	}

	store2.Clear("c1", "t1")
	store3, err := NewSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(store3.Get("c1", "t1", 10)); got != 0 {
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
	store.Append("c", "t", llm.Message{Role: "user", Content: "x"}) // should not panic
}

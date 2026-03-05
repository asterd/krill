// Package memory — per-thread conversation history management.
//
// Provides:
//   - In-RAM ring buffer (configurable window size)
//   - Thread isolation: each clientID+threadID has its own history
//   - Store interface for swapping in persistent backends (sqlite, redis, etc.)
package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/telemetry"
	_ "modernc.org/sqlite"
)

// Store is the persistence interface.
// The default implementation is sqlite on local disk; RAM/file remain available.
type Store interface {
	// Append adds a message to the thread history.
	Append(clientID, threadID string, msg llm.Message)
	// Get returns the last `window` messages for a thread.
	Get(clientID, threadID string, window int) []llm.Message
	// Clear wipes a thread's history.
	Clear(clientID, threadID string)
}

func NewStore(backend, path string) (Store, error) {
	switch backend {
	case "", "sqlite":
		return NewSQLite(path)
	case "ram":
		return NewRAM(), nil
	case "file":
		return NewFile(path)
	default:
		return nil, fmt.Errorf("unknown memory backend %q", backend)
	}
}

// ─── In-RAM store ─────────────────────────────────────────────────────────────

type thread struct {
	msgs      []llm.Message
	updatedAt time.Time
}

type ramStore struct {
	mu      sync.RWMutex
	threads map[string]*thread // key: clientID+":"+threadID
}

func NewRAM() Store {
	return &ramStore{threads: make(map[string]*thread)}
}

func key(clientID, threadID string) string { return clientID + ":" + threadID }

func (s *ramStore) Append(clientID, threadID string, msg llm.Message) {
	span := telemetry.StartSpan(nil, "", "", "memory.append", "backend", "ram")
	defer span.End(nil, "backend", "ram")
	k := key(clientID, threadID)
	s.mu.Lock()
	t, ok := s.threads[k]
	if !ok {
		t = &thread{}
		s.threads[k] = t
	}
	t.msgs = append(t.msgs, msg)
	t.updatedAt = time.Now()
	size := estimateMessageBytes(msg)
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "append", "backend": "ram"})
	telemetry.SetGauge(telemetry.MetricMemoryBytes, int64(size), map[string]string{"backend": "ram"})
	s.mu.Unlock()
}

func (s *ramStore) Get(clientID, threadID string, window int) []llm.Message {
	span := telemetry.StartSpan(nil, "", "", "memory.get", "backend", "ram")
	defer span.End(nil, "backend", "ram")
	k := key(clientID, threadID)
	s.mu.RLock()
	t, ok := s.threads[k]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	s.mu.RLock()
	msgs := append([]llm.Message(nil), t.msgs...)
	s.mu.RUnlock()
	if window > 0 && len(msgs) > window {
		telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "get", "backend": "ram"})
		return msgs[len(msgs)-window:]
	}
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "get", "backend": "ram"})
	return msgs
}

func (s *ramStore) Clear(clientID, threadID string) {
	span := telemetry.StartSpan(nil, "", "", "memory.clear", "backend", "ram")
	defer span.End(nil, "backend", "ram")
	s.mu.Lock()
	delete(s.threads, key(clientID, threadID))
	s.mu.Unlock()
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "clear", "backend": "ram"})
}

// ─── File-backed store (durable across process restarts) ────────────────────

type fileStore struct {
	mu   sync.RWMutex
	path string
	data map[string][]llm.Message
}

func NewFile(path string) (Store, error) {
	if path == "" {
		path = "./.krill/memory.json"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir memory dir: %w", err)
	}
	s := &fileStore{path: path, data: map[string][]llm.Message{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *fileStore) Append(clientID, threadID string, msg llm.Message) {
	span := telemetry.StartSpan(nil, "", "", "memory.append", "backend", "file")
	defer span.End(nil, "backend", "file")
	s.mu.Lock()
	k := key(clientID, threadID)
	s.data[k] = append(s.data[k], msg)
	_ = s.persistLocked()
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "append", "backend": "file"})
	telemetry.SetGauge(telemetry.MetricMemoryBytes, int64(estimateMessageBytes(msg)), map[string]string{"backend": "file"})
	s.mu.Unlock()
}

func (s *fileStore) Get(clientID, threadID string, window int) []llm.Message {
	span := telemetry.StartSpan(nil, "", "", "memory.get", "backend", "file")
	defer span.End(nil, "backend", "file")
	s.mu.RLock()
	msgs := append([]llm.Message(nil), s.data[key(clientID, threadID)]...)
	s.mu.RUnlock()
	if window > 0 && len(msgs) > window {
		telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "get", "backend": "file"})
		return msgs[len(msgs)-window:]
	}
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "get", "backend": "file"})
	return msgs
}

func (s *fileStore) Clear(clientID, threadID string) {
	span := telemetry.StartSpan(nil, "", "", "memory.clear", "backend", "file")
	defer span.End(nil, "backend", "file")
	s.mu.Lock()
	delete(s.data, key(clientID, threadID))
	_ = s.persistLocked()
	s.mu.Unlock()
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "clear", "backend": "file"})
}

func (s *fileStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read memory file: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	var decoded map[string][]llm.Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("decode memory file: %w", err)
	}
	s.data = decoded
	if s.data == nil {
		s.data = map[string][]llm.Message{}
	}
	return nil
}

func (s *fileStore) persistLocked() error {
	payload, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode memory file: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return fmt.Errorf("write temp memory file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace memory file: %w", err)
	}
	return nil
}

// ─── SQLite-backed store (durable and lightweight) ──────────────────────────

type sqliteStore struct {
	db *sql.DB
}

func NewSQLite(path string) (Store, error) {
	if path == "" {
		path = "./.krill/memory.db"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir memory dir: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	const schema = `
CREATE TABLE IF NOT EXISTS memory_messages (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  client_id TEXT NOT NULL,
  thread_id TEXT NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
  tool_calls_json TEXT NOT NULL DEFAULT '[]',
  tool_call_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX IF NOT EXISTS idx_memory_thread_seq ON memory_messages(client_id, thread_id, seq);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init sqlite schema: %w", err)
	}
	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) Append(clientID, threadID string, msg llm.Message) {
	span := telemetry.StartSpan(nil, "", "", "memory.append", "backend", "sqlite")
	defer span.End(nil, "backend", "sqlite")
	toolCalls, err := json.Marshal(msg.ToolCalls)
	if err != nil {
		toolCalls = []byte("[]")
	}
	_, _ = s.db.Exec(
		`INSERT INTO memory_messages (client_id, thread_id, role, content, tool_calls_json, tool_call_id) VALUES (?, ?, ?, ?, ?, ?)`,
		clientID, threadID, msg.Role, msg.Content, string(toolCalls), msg.ToolCallID,
	)
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "append", "backend": "sqlite"})
	telemetry.SetGauge(telemetry.MetricMemoryBytes, int64(estimateMessageBytes(msg)), map[string]string{"backend": "sqlite"})
}

func (s *sqliteStore) Get(clientID, threadID string, window int) []llm.Message {
	span := telemetry.StartSpan(nil, "", "", "memory.get", "backend", "sqlite")
	defer span.End(nil, "backend", "sqlite")
	limit := window
	if limit <= 0 {
		limit = 1000000
	}
	rows, err := s.db.Query(
		`SELECT role, content, tool_calls_json, tool_call_id FROM memory_messages
		 WHERE client_id = ? AND thread_id = ?
		 ORDER BY seq DESC
		 LIMIT ?`,
		clientID, threadID, limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var reversed []llm.Message
	for rows.Next() {
		var (
			role, content, toolCallsJSON, toolCallID string
			msg                                      llm.Message
		)
		if err := rows.Scan(&role, &content, &toolCallsJSON, &toolCallID); err != nil {
			continue
		}
		msg.Role = role
		msg.Content = content
		msg.ToolCallID = toolCallID
		_ = json.Unmarshal([]byte(toolCallsJSON), &msg.ToolCalls)
		reversed = append(reversed, msg)
	}
	msgs := make([]llm.Message, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		msgs = append(msgs, reversed[i])
	}
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "get", "backend": "sqlite"})
	return msgs
}

func (s *sqliteStore) Clear(clientID, threadID string) {
	span := telemetry.StartSpan(nil, "", "", "memory.clear", "backend", "sqlite")
	defer span.End(nil, "backend", "sqlite")
	_, _ = s.db.Exec(`DELETE FROM memory_messages WHERE client_id = ? AND thread_id = ?`, clientID, threadID)
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "clear", "backend": "sqlite"})
}

func estimateMessageBytes(msg llm.Message) int {
	total := len(msg.Role) + len(msg.Content) + len(msg.ToolCallID)
	for _, tc := range msg.ToolCalls {
		total += len(tc.ID) + len(tc.Type) + len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return total
}

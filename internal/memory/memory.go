// Package memory manages hot conversation buffers and optional durable history backends.
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

// Snapshot is a restorable thread buffer image.
type Snapshot struct {
	Messages  []llm.Message `json:"messages"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// Store is the conversation history contract used by the runtime kernel.
type Store interface {
	Append(clientID, threadID string, msg llm.Message) error
	AppendBatch(clientID, threadID string, msgs []llm.Message) error
	Get(clientID, threadID string, window int) ([]llm.Message, error)
	Snapshot(clientID, threadID string) (Snapshot, error)
	Restore(clientID, threadID string, snapshot Snapshot) error
	Trim(clientID, threadID string, keep int) error
	Clear(clientID, threadID string) error
}

// NewStore creates the configured memory backend.
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

type thread struct {
	msgs      []llm.Message
	updatedAt time.Time
}

func key(clientID, threadID string) string { return clientID + ":" + threadID }

type ramStore struct {
	mu      sync.RWMutex
	threads map[string]*thread
}

// NewRAM creates a non-persistent in-memory history store.
func NewRAM() Store {
	return &ramStore{threads: make(map[string]*thread)}
}

func (s *ramStore) Append(clientID, threadID string, msg llm.Message) error {
	return s.AppendBatch(clientID, threadID, []llm.Message{msg})
}

func (s *ramStore) AppendBatch(clientID, threadID string, msgs []llm.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	k := key(clientID, threadID)
	s.mu.Lock()
	t, ok := s.threads[k]
	if !ok {
		t = &thread{}
		s.threads[k] = t
	}
	t.msgs = append(t.msgs, cloneMessages(msgs)...)
	t.updatedAt = time.Now().UTC()
	size := 0
	for _, msg := range msgs {
		size += estimateMessageBytes(msg)
	}
	s.mu.Unlock()
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "append_batch", "backend": "ram"})
	telemetry.SetGauge(telemetry.MetricMemoryBytes, int64(size), map[string]string{"backend": "ram"})
	return nil
}

func (s *ramStore) Get(clientID, threadID string, window int) ([]llm.Message, error) {
	s.mu.RLock()
	t, ok := s.threads[key(clientID, threadID)]
	if !ok {
		s.mu.RUnlock()
		return nil, nil
	}
	msgs := cloneMessages(t.msgs)
	s.mu.RUnlock()
	if window > 0 && len(msgs) > window {
		msgs = msgs[len(msgs)-window:]
	}
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "get", "backend": "ram"})
	return msgs, nil
}

func (s *ramStore) Snapshot(clientID, threadID string) (Snapshot, error) {
	s.mu.RLock()
	t, ok := s.threads[key(clientID, threadID)]
	if !ok {
		s.mu.RUnlock()
		return Snapshot{}, nil
	}
	snap := Snapshot{Messages: cloneMessages(t.msgs), UpdatedAt: t.updatedAt}
	s.mu.RUnlock()
	return snap, nil
}

func (s *ramStore) Restore(clientID, threadID string, snapshot Snapshot) error {
	s.mu.Lock()
	s.threads[key(clientID, threadID)] = &thread{
		msgs:      cloneMessages(snapshot.Messages),
		updatedAt: snapshot.UpdatedAt,
	}
	s.mu.Unlock()
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "restore", "backend": "ram"})
	return nil
}

func (s *ramStore) Trim(clientID, threadID string, keep int) error {
	if keep < 0 {
		return fmt.Errorf("keep must be >= 0")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.threads[key(clientID, threadID)]
	if !ok || len(t.msgs) <= keep || keep == 0 && len(t.msgs) == 0 {
		return nil
	}
	if keep == 0 {
		t.msgs = nil
	} else {
		t.msgs = append([]llm.Message(nil), t.msgs[len(t.msgs)-keep:]...)
	}
	t.updatedAt = time.Now().UTC()
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "trim", "backend": "ram"})
	return nil
}

func (s *ramStore) Clear(clientID, threadID string) error {
	s.mu.Lock()
	delete(s.threads, key(clientID, threadID))
	s.mu.Unlock()
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "clear", "backend": "ram"})
	return nil
}

type fileStore struct {
	mu   sync.RWMutex
	path string
	data map[string]Snapshot
}

// NewFile creates a JSON file-backed history store.
func NewFile(path string) (Store, error) {
	if path == "" {
		path = "./.krill/memory.json"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir memory dir: %w", err)
	}
	s := &fileStore{path: path, data: map[string]Snapshot{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *fileStore) Append(clientID, threadID string, msg llm.Message) error {
	return s.AppendBatch(clientID, threadID, []llm.Message{msg})
}

func (s *fileStore) AppendBatch(clientID, threadID string, msgs []llm.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	s.mu.Lock()
	k := key(clientID, threadID)
	snap := s.data[k]
	snap.Messages = append(snap.Messages, cloneMessages(msgs)...)
	snap.UpdatedAt = time.Now().UTC()
	s.data[k] = snap
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "append_batch", "backend": "file"})
	return nil
}

func (s *fileStore) Get(clientID, threadID string, window int) ([]llm.Message, error) {
	s.mu.RLock()
	msgs := cloneMessages(s.data[key(clientID, threadID)].Messages)
	s.mu.RUnlock()
	if window > 0 && len(msgs) > window {
		msgs = msgs[len(msgs)-window:]
	}
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "get", "backend": "file"})
	return msgs, nil
}

func (s *fileStore) Snapshot(clientID, threadID string) (Snapshot, error) {
	s.mu.RLock()
	snap := cloneSnapshot(s.data[key(clientID, threadID)])
	s.mu.RUnlock()
	return snap, nil
}

func (s *fileStore) Restore(clientID, threadID string, snapshot Snapshot) error {
	s.mu.Lock()
	s.data[key(clientID, threadID)] = cloneSnapshot(snapshot)
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "restore", "backend": "file"})
	return nil
}

func (s *fileStore) Trim(clientID, threadID string, keep int) error {
	if keep < 0 {
		return fmt.Errorf("keep must be >= 0")
	}
	s.mu.Lock()
	k := key(clientID, threadID)
	snap := s.data[k]
	if keep == 0 {
		snap.Messages = nil
	} else if len(snap.Messages) > keep {
		snap.Messages = append([]llm.Message(nil), snap.Messages[len(snap.Messages)-keep:]...)
	}
	snap.UpdatedAt = time.Now().UTC()
	s.data[k] = snap
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "trim", "backend": "file"})
	return nil
}

func (s *fileStore) Clear(clientID, threadID string) error {
	s.mu.Lock()
	delete(s.data, key(clientID, threadID))
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "clear", "backend": "file"})
	return nil
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
	var decoded map[string]Snapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("decode memory file: %w", err)
	}
	s.data = decoded
	if s.data == nil {
		s.data = map[string]Snapshot{}
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

type sqliteStore struct {
	db *sql.DB
}

// NewSQLite creates a SQLite-backed durable history store.
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

func (s *sqliteStore) Append(clientID, threadID string, msg llm.Message) error {
	return s.AppendBatch(clientID, threadID, []llm.Message{msg})
}

func (s *sqliteStore) AppendBatch(clientID, threadID string, msgs []llm.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin sqlite append: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO memory_messages (client_id, thread_id, role, content, tool_calls_json, tool_call_id) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare sqlite append: %w", err)
	}
	defer stmt.Close()
	for _, msg := range msgs {
		toolCalls, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			toolCalls = []byte("[]")
		}
		if _, err := stmt.Exec(clientID, threadID, msg.Role, msg.Content, string(toolCalls), msg.ToolCallID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("exec sqlite append: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sqlite append: %w", err)
	}
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "append_batch", "backend": "sqlite"})
	return nil
}

func (s *sqliteStore) Get(clientID, threadID string, window int) ([]llm.Message, error) {
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
		return nil, fmt.Errorf("query sqlite get: %w", err)
	}
	defer rows.Close()

	var reversed []llm.Message
	for rows.Next() {
		var (
			role, content, toolCallsJSON, toolCallID string
			msg                                      llm.Message
		)
		if err := rows.Scan(&role, &content, &toolCallsJSON, &toolCallID); err != nil {
			return nil, fmt.Errorf("scan sqlite get: %w", err)
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
	return msgs, nil
}

func (s *sqliteStore) Snapshot(clientID, threadID string) (Snapshot, error) {
	msgs, err := s.Get(clientID, threadID, 0)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Messages: msgs, UpdatedAt: time.Now().UTC()}, nil
}

func (s *sqliteStore) Restore(clientID, threadID string, snapshot Snapshot) error {
	if err := s.Clear(clientID, threadID); err != nil {
		return err
	}
	return s.AppendBatch(clientID, threadID, snapshot.Messages)
}

func (s *sqliteStore) Trim(clientID, threadID string, keep int) error {
	if keep < 0 {
		return fmt.Errorf("keep must be >= 0")
	}
	if keep == 0 {
		return s.Clear(clientID, threadID)
	}
	_, err := s.db.Exec(
		`DELETE FROM memory_messages
		  WHERE client_id = ? AND thread_id = ? AND seq NOT IN (
		    SELECT seq FROM memory_messages
		     WHERE client_id = ? AND thread_id = ?
		     ORDER BY seq DESC
		     LIMIT ?
		  )`,
		clientID, threadID, clientID, threadID, keep,
	)
	if err != nil {
		return fmt.Errorf("trim sqlite messages: %w", err)
	}
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "trim", "backend": "sqlite"})
	return nil
}

func (s *sqliteStore) Clear(clientID, threadID string) error {
	if _, err := s.db.Exec(`DELETE FROM memory_messages WHERE client_id = ? AND thread_id = ?`, clientID, threadID); err != nil {
		return fmt.Errorf("clear sqlite messages: %w", err)
	}
	telemetry.IncCounter(telemetry.MetricMemoryOpsTotal, 1, map[string]string{"op": "clear", "backend": "sqlite"})
	return nil
}

func (s *sqliteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func cloneMessages(in []llm.Message) []llm.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]llm.Message, len(in))
	copy(out, in)
	return out
}

func cloneSnapshot(in Snapshot) Snapshot {
	return Snapshot{Messages: cloneMessages(in.Messages), UpdatedAt: in.UpdatedAt}
}

func estimateMessageBytes(msg llm.Message) int {
	total := len(msg.Role) + len(msg.Content) + len(msg.ToolCallID)
	for _, tc := range msg.ToolCalls {
		total += len(tc.ID) + len(tc.Type) + len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return total
}

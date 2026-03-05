// Package memory — per-thread conversation history management.
//
// Provides:
//   - In-RAM ring buffer (configurable window size)
//   - Thread isolation: each clientID+threadID has its own history
//   - Store interface for swapping in persistent backends (sqlite, redis, etc.)
package memory

import (
	"sync"
	"time"

	"github.com/krill/krill/internal/llm"
)

// Store is the persistence interface.
// The default implementation is in-process RAM; replace with sqlite/redis for persistence.
type Store interface {
	// Append adds a message to the thread history.
	Append(clientID, threadID string, msg llm.Message)
	// Get returns the last `window` messages for a thread.
	Get(clientID, threadID string, window int) []llm.Message
	// Clear wipes a thread's history.
	Clear(clientID, threadID string)
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
	k := key(clientID, threadID)
	s.mu.Lock()
	t, ok := s.threads[k]
	if !ok {
		t = &thread{}
		s.threads[k] = t
	}
	t.msgs = append(t.msgs, msg)
	t.updatedAt = time.Now()
	s.mu.Unlock()
}

func (s *ramStore) Get(clientID, threadID string, window int) []llm.Message {
	k := key(clientID, threadID)
	s.mu.RLock()
	t, ok := s.threads[k]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	s.mu.RLock()
	msgs := t.msgs
	s.mu.RUnlock()
	if window > 0 && len(msgs) > window {
		return msgs[len(msgs)-window:]
	}
	return msgs
}

func (s *ramStore) Clear(clientID, threadID string) {
	s.mu.Lock()
	delete(s.threads, key(clientID, threadID))
	s.mu.Unlock()
}

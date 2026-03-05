package session

import (
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
)

// MemoryStore decorates a base memory store with session persistence hooks.
type MemoryStore struct {
	base memory.Store
	svc  *Service
}

// Hydrator restores persisted messages into a live memory backend without
// re-emitting session persistence side effects.
type Hydrator interface {
	Hydrate(clientID, threadID string, msgs []llm.Message)
}

// WrapMemoryStore decorates a memory store so session persistence is updated on writes.
func WrapMemoryStore(base memory.Store, svc *Service) memory.Store {
	if base == nil || svc == nil {
		return base
	}
	return &MemoryStore{base: base, svc: svc}
}

func (m *MemoryStore) Append(clientID, threadID string, msg llm.Message) {
	m.base.Append(clientID, threadID, msg)
	_ = m.svc.RecordMessage(clientID, threadID, msg, Provenance{Actor: "memory", Source: "memory.append"})
}

func (m *MemoryStore) Get(clientID, threadID string, window int) []llm.Message {
	return m.base.Get(clientID, threadID, window)
}

func (m *MemoryStore) Clear(clientID, threadID string) {
	m.base.Clear(clientID, threadID)
}

func (m *MemoryStore) Hydrate(clientID, threadID string, msgs []llm.Message) {
	for _, msg := range msgs {
		m.base.Append(clientID, threadID, msg)
	}
}

func (m *MemoryStore) Close() error {
	if closer, ok := m.base.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

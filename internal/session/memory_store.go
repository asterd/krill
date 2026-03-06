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

func (m *MemoryStore) Append(clientID, threadID string, msg llm.Message) error {
	if err := m.base.Append(clientID, threadID, msg); err != nil {
		return err
	}
	return m.svc.RecordMessageAsync(clientID, threadID, msg, Provenance{Actor: "memory", Source: "memory.append"})
}

func (m *MemoryStore) AppendBatch(clientID, threadID string, msgs []llm.Message) error {
	if err := m.base.AppendBatch(clientID, threadID, msgs); err != nil {
		return err
	}
	for _, msg := range msgs {
		if err := m.svc.RecordMessageAsync(clientID, threadID, msg, Provenance{Actor: "memory", Source: "memory.append_batch"}); err != nil {
			return err
		}
	}
	return nil
}

func (m *MemoryStore) Get(clientID, threadID string, window int) ([]llm.Message, error) {
	return m.base.Get(clientID, threadID, window)
}

func (m *MemoryStore) Snapshot(clientID, threadID string) (memory.Snapshot, error) {
	return m.base.Snapshot(clientID, threadID)
}

func (m *MemoryStore) Restore(clientID, threadID string, snapshot memory.Snapshot) error {
	return m.base.Restore(clientID, threadID, snapshot)
}

func (m *MemoryStore) Trim(clientID, threadID string, keep int) error {
	return m.base.Trim(clientID, threadID, keep)
}

func (m *MemoryStore) Clear(clientID, threadID string) error {
	return m.base.Clear(clientID, threadID)
}

func (m *MemoryStore) Hydrate(clientID, threadID string, msgs []llm.Message) {
	_ = m.base.AppendBatch(clientID, threadID, msgs)
}

func (m *MemoryStore) Close() error {
	if closer, ok := m.base.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

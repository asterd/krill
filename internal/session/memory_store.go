package session

import (
	"github.com/krill/krill/internal/llm"
	"github.com/krill/krill/internal/memory"
)

type MemoryStore struct {
	base memory.Store
	svc  *Service
}

type Hydrator interface {
	Hydrate(clientID, threadID string, msgs []llm.Message)
}

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

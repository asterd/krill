package pubsub

import (
	"sync"
	"time"
)

// DedupStore tracks recently seen message keys for at-least-once transports.
type DedupStore struct {
	ttl  time.Duration
	mu   sync.Mutex
	seen map[string]time.Time
}

// NewDedupStore creates a TTL-based deduplication cache for PubSub message keys.
func NewDedupStore(ttl time.Duration) *DedupStore {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &DedupStore{ttl: ttl, seen: make(map[string]time.Time)}
}

func (d *DedupStore) SeenBefore(key string, now time.Time) bool {
	if key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gc(now)
	if exp, ok := d.seen[key]; ok {
		return exp.After(now)
	}
	return false
}

func (d *DedupStore) Mark(key string, now time.Time) {
	if key == "" {
		return
	}
	d.mu.Lock()
	d.gc(now)
	d.seen[key] = now.Add(d.ttl)
	d.mu.Unlock()
}

func (d *DedupStore) gc(now time.Time) {
	for k, exp := range d.seen {
		if !exp.After(now) {
			delete(d.seen, k)
		}
	}
}

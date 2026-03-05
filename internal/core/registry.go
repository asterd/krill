// Package core — plugin registry with zero circular imports.
//
// The registry stores only constructor functions (factories).
// It imports no internal packages — all factory signatures use
// primitive types or interfaces defined in this package.
// Plugins import core to register; core never imports plugins.
package core

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/krill/krill/internal/bus"
)

// ─── Protocol plugin interface ────────────────────────────────────────────────

// Protocol is the interface all ingress adapters must implement.
type Protocol interface {
	Name() string
	// Start begins listening. Must be non-blocking (spawn goroutines internally).
	// b is the bus to publish inbound messages to and subscribe replies from.
	Start(ctx context.Context, b bus.Bus, log *slog.Logger) error
	Stop(ctx context.Context) error
}

// ProtocolFactory constructs a Protocol from a config map.
type ProtocolFactory func(cfg map[string]interface{}) (Protocol, error)

// ─── Registry ─────────────────────────────────────────────────────────────────

var global = newRegistry()

type registry struct {
	mu        sync.RWMutex
	protocols map[string]ProtocolFactory
}

func newRegistry() *registry {
	return &registry{protocols: make(map[string]ProtocolFactory)}
}

// Global returns the singleton plugin registry.
func Global() *registry { return global }

// RegisterProtocol adds a protocol factory. Called from plugin init() functions.
func (r *registry) RegisterProtocol(name string, f ProtocolFactory) {
	r.mu.Lock()
	r.protocols[name] = f
	r.mu.Unlock()
}

// BuildProtocol instantiates a registered protocol by name.
func (r *registry) BuildProtocol(name string, cfg map[string]interface{}) (Protocol, error) {
	r.mu.RLock()
	f, ok := r.protocols[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown protocol plugin %q (did you import it?)", name)
	}
	return f(cfg)
}

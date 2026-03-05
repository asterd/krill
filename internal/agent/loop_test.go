package agent

import (
	"io"
	"log/slog"
	"testing"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/bus"
	"github.com/krill/krill/internal/memory"
)

func TestNew_UsesConfiguredMemoryWindow(t *testing.T) {
	loop := New(config.AgentConfig{Name: "a"}, bus.NewLocal(1), memory.NewRAM(), 7, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if loop.memWindow != 7 {
		t.Fatalf("expected memory window 7, got %d", loop.memWindow)
	}
}

func TestNew_DefaultsMemoryWindow(t *testing.T) {
	loop := New(config.AgentConfig{Name: "a"}, bus.NewLocal(1), memory.NewRAM(), 0, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if loop.memWindow != 100 {
		t.Fatalf("expected default memory window 100, got %d", loop.memWindow)
	}
}

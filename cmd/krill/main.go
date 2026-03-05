// Krill — one binary, zero magic.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/krill/krill/config"
	"github.com/krill/krill/internal/core"

	// ── Protocol plugins (self-register via init()) ──────────────────────────
	_ "github.com/krill/krill/plugins/protocol/http"
	_ "github.com/krill/krill/plugins/protocol/pubsub"
	_ "github.com/krill/krill/plugins/protocol/telegram"
	_ "github.com/krill/krill/plugins/protocol/webhook"
	// _ "github.com/krill/krill/plugins/protocol/mcp"     // uncomment to enable
	// _ "github.com/krill/krill/plugins/protocol/discord"
	// _ "github.com/krill/krill/plugins/protocol/pubsub"
)

func main() {
	cfgPath := flag.String("config", "krill.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	log := newLogger(cfg.Core.LogFormat)
	slog.SetDefault(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine, err := core.New(cfg, log)
	if err != nil {
		slog.Error("engine init", "err", err)
		os.Exit(1)
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		slog.Info("signal received, shutting down")
		cancel()
	}()

	if err := engine.Run(ctx); err != nil {
		slog.Error("engine", "err", err)
		os.Exit(1)
	}
}

func newLogger(format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	case "text", "txt", "console":
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	default:
		log := slog.New(slog.NewJSONHandler(os.Stdout, opts))
		log.Warn("unknown log format, using json", "log_format", format)
		return log
	}
}

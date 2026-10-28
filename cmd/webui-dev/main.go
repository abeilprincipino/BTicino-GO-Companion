// Command webui-dev serves the real web UI on a development machine, without an
// intercom: no SIP, no OpenWebNet, no device metadata. Restart and reboot do
// nothing, and the pages that read the intercom (logs, bus frames,
// diagnostics, update) come up empty.
package main

import (
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/webui"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// devMetadata stands in for what the companion reads from the intercom's own
// files at startup.
var devMetadata = config.Metadata{Model: "C100X", MAC: "00:11:22:33:44:55"}

func main() {
	os.Exit(run())
}

func run() int {
	var configPath, addr string
	flag.StringVar(&configPath, "config", filepath.Join(os.TempDir(), "companion-webui-dev", "config.yaml"), "path to config.yaml, created if missing")
	flag.StringVar(&addr, "addr", ":8081", "listen address")
	flag.Parse()

	level := new(slog.LevelVar)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	store, err := openConfig(configPath)
	if err != nil {
		logger.Error("open config failed", "path", configPath, "error", err)
		return 1
	}

	authStore := auth.NewStore(store)
	authStore.SetLogger(logger.With("component", "auth"))

	noop := func(context.Context) error {
		logger.Info("ignored: no intercom in development")
		return nil
	}

	server := webui.New(store, authStore, logger, noop, noop, func(name string) error {
		return level.UnmarshalText([]byte(name))
	})

	httpServer := &http.Server{Addr: addr, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("web UI ready", "url", "http://localhost"+addr, "config", configPath)

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("serve failed", "error", err)
		return 1
	}

	return 0
}

func openConfig(path string) (*config.Store, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create config directory: %w", err)
		}

		if _, err := config.Create(path, devMetadata); err != nil {
			return nil, fmt.Errorf("create config: %w", err)
		}
	}

	store, err := config.Open(path)
	if err != nil {
		return nil, err
	}

	if err := store.ApplyMetadata(devMetadata); err != nil {
		return nil, fmt.Errorf("apply metadata: %w", err)
	}

	return store, nil
}

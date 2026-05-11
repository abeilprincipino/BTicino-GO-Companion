package app

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunReturnsLoadConfigErrorOnInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte("{invalid"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := Run(context.Background(), cfgPath, log.New(io.Discard, "", 0))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Fatalf("unexpected error: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancellation error: %v", err)
	}
}

func TestLoadOrCreateConfigCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, created, err := loadOrCreateConfig(path)
	if err != nil {
		t.Fatalf("loadOrCreateConfig failed: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true")
	}
	if strings.TrimSpace(cfg.ClaimCode) == "" {
		t.Fatalf("expected generated claim code")
	}
	if cfg.DataDir != filepath.Dir(path) {
		t.Fatalf("unexpected data dir: %s", cfg.DataDir)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected config file to exist: %v", err)
	}
}

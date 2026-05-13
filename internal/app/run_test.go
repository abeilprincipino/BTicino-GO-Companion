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

func TestLoadOrCreateConfigUsesConfiguredModelWhenMetadataUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
  "auth": {
    "claim_code": "abcd-1234"
  },
  "entrypoints": [
    {
      "id": "main",
      "label": "Main Gate",
      "devaddr": "20",
      "has_stream": true,
      "has_unlock": true,
      "has_ring": true
    }
  ],
  "device_model": "C300X"
}
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, created, err := loadOrCreateConfig(path)
	if err != nil {
		t.Fatalf("loadOrCreateConfig failed: %v", err)
	}
	if created {
		t.Fatalf("expected created=false")
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

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
  "schema_version": 2,
  "system": {
    "control": {
      "reboot": {
        "enabled": true
      }
    },
    "services": {
      "dropbear": {
        "enabled": true,
        "exposed": true
      }
    }
  },
  "intercom": {
    "info": {
      "model": "C300X"
    },
    "auth": {
      "claim_code": "abcd-1234"
    },
    "config": {
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
      "audio": {
        "enabled": true,
        "exposed": true
      },
      "voicemail": {
        "messages_dir": "/home/bticino/cfg/extra/47/messages",
        "enabled": true,
        "exposed": true
      }
    }
  }
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

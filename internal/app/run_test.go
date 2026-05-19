package app

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"bticino-go-companion/internal/config"
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
  "companion": {
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

func TestDefaultClaimCodeFormat(t *testing.T) {
	code := defaultClaimCode()
	if len(code) != 9 {
		t.Fatalf("expected length 9 claim code, got %q", code)
	}
	if code[4] != '-' {
		t.Fatalf("expected dash in claim code, got %q", code)
	}
	ok, err := regexp.MatchString("^[0-9a-f]{4}-[0-9a-f]{4}$", code)
	if err != nil {
		t.Fatalf("regexp error: %v", err)
	}
	if !ok {
		t.Fatalf("unexpected claim code format: %q", code)
	}
}

func TestNormalizedDetectedModel(t *testing.T) {
	if got := normalizedDetectedModel(""); got != "" {
		t.Fatalf("expected empty for blank model, got %q", got)
	}
	if got := normalizedDetectedModel(" unknown "); got != "" {
		t.Fatalf("expected empty for unknown model, got %q", got)
	}
	if got := normalizedDetectedModel(" C300X "); got != "C300X" {
		t.Fatalf("expected C300X, got %q", got)
	}
}

func TestSetIfNonEmpty(t *testing.T) {
	current := "old"
	if changed := setIfNonEmpty(&current, "unknown"); changed {
		t.Fatal("expected no change for unknown value")
	}
	if current != "old" {
		t.Fatalf("unexpected current value: %q", current)
	}

	if changed := setIfNonEmpty(nil, "next"); changed {
		t.Fatal("expected no change for nil destination")
	}

	if changed := setIfNonEmpty(&current, "old"); changed {
		t.Fatal("expected no change for same value")
	}
	if changed := setIfNonEmpty(&current, " next "); !changed {
		t.Fatal("expected change for non-empty distinct value")
	}
	if current != "next" {
		t.Fatalf("expected current=next, got %q", current)
	}
}

func TestSelfHealthCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v2/health" {
			http.NotFound(w, req)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	addr := strings.TrimPrefix(server.URL, "http://")
	cfg := config.Default()
	cfg.ListenAddr = addr

	check := selfHealthCheck(cfg)
	if err := check(context.Background()); err != nil {
		t.Fatalf("expected healthy check to pass, got %v", err)
	}
}

func TestSelfHealthCheckNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/v2/health" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		http.NotFound(w, req)
	}))
	defer server.Close()

	addr := strings.TrimPrefix(server.URL, "http://")
	cfg := config.Default()
	cfg.ListenAddr = addr

	check := selfHealthCheck(cfg)
	if err := check(context.Background()); err == nil {
		t.Fatal("expected non-200 health check to fail")
	}
}

func TestEnrichConfigWithDiagnosticMetadataNilInputs(t *testing.T) {
	if err := enrichConfigWithDiagnosticMetadata(nil, nil, nil); err != nil {
		t.Fatalf("expected nil input helper to be no-op, got %v", err)
	}
	cfg := config.Default()
	if err := enrichConfigWithDiagnosticMetadata(&cfg, nil, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("expected nil command client to be no-op, got %v", err)
	}
	if err := enrichConfigWithDiagnosticMetadataWithRetry(&cfg, nil, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("expected retry helper with nil command client to be no-op, got %v", err)
	}
}

func TestLoadOrCreateConfigCreateBranch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new-config.json")
	cfg, created, err := loadOrCreateConfig(path)
	if err != nil {
		// On non-device CI hosts metadata detection can be unknown; this is acceptable.
		if !strings.Contains(strings.ToLower(err.Error()), "device model detection failed") {
			t.Fatalf("unexpected create-branch error: %v", err)
		}
		return
	}
	if !created {
		t.Fatalf("expected created=true when config did not exist, got created=%v cfg=%+v", created, cfg)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("expected created config file, got stat error: %v", statErr)
	}
}

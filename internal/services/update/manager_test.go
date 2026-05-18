package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bticino-go-companion/internal/config"
)

func TestApplyAndRollbackLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	cfg := config.Default()
	cfg.Version = "1.0.0"
	cfg.DataDir = filepath.Join(tempDir, "companion")
	cfg.UpdateAllowSelfRestart = false

	current := cfg.UpdateBinCurrentPath()
	if err := os.MkdirAll(filepath.Dir(current), 0o755); err != nil {
		t.Fatalf("mkdir current dir: %v", err)
	}
	if err := os.WriteFile(current, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write current: %v", err)
	}

	candidatePath := filepath.Join(tempDir, "candidate.bin")
	if err := os.WriteFile(candidatePath, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	m := NewManager(cfg, log.New(&bytes.Buffer{}, "", 0), nil)
	applyStatus, err := m.Apply(&Artifact{Version: "1.1.0", Path: candidatePath})
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if applyStatus.Stage != StageHealthy {
		t.Fatalf("expected healthy stage, got %s", applyStatus.Stage)
	}
	if !applyStatus.RestartRequired {
		t.Fatalf("expected restart_required=true after apply")
	}
	if applyStatus.CurrentVersion != "1.1.0" {
		t.Fatalf("expected current version 1.1.0, got %s", applyStatus.CurrentVersion)
	}

	gotCurrent, _ := os.ReadFile(cfg.UpdateBinCurrentPath())
	if string(gotCurrent) != "new-binary" {
		t.Fatalf("unexpected current binary payload: %s", string(gotCurrent))
	}
	gotPrevious, _ := os.ReadFile(cfg.UpdateBinPreviousPath())
	if string(gotPrevious) != "old-binary" {
		t.Fatalf("unexpected previous binary payload: %s", string(gotPrevious))
	}

	rollbackStatus, err := m.Rollback()
	if err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	if rollbackStatus.Stage != StageHealthy {
		t.Fatalf("expected healthy stage after rollback, got %s", rollbackStatus.Stage)
	}
	if rollbackStatus.CurrentVersion != "1.0.0" {
		t.Fatalf("expected rollback current version to 1.0.0, got %s", rollbackStatus.CurrentVersion)
	}
	gotCurrentAfterRollback, _ := os.ReadFile(cfg.UpdateBinCurrentPath())
	if string(gotCurrentAfterRollback) != "old-binary" {
		t.Fatalf("expected current binary restored to old payload, got %s", string(gotCurrentAfterRollback))
	}
}

func TestCheckFromGitHubRelease(t *testing.T) {
	tempDir := t.TempDir()
	cfg := config.Default()
	cfg.Version = "v0.0.1"
	cfg.DataDir = filepath.Join(tempDir, "companion")
	cfg.UpdateManifestPath = ""
	cfg.UpdateReleaseRepo = "owner/repo"
	cfg.UpdateReleaseAsset = "companion"

	shaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 64) + "  companion\n"))
	}))
	defer shaServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/releases/latest" {
			http.NotFound(w, r)
			return
		}
		resp := map[string]any{
			"tag_name": "v0.0.2",
			"assets": []map[string]any{
				{"name": "companion", "browser_download_url": "https://example.invalid/companion"},
				{"name": "companion.sha256", "browser_download_url": shaServer.URL + "/companion.sha256"},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer apiServer.Close()

	cfg.UpdateReleaseAPI = apiServer.URL

	m := NewManager(cfg, log.New(&bytes.Buffer{}, "", 0), nil)
	status, err := m.Check(nil)
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}
	if status.Stage != StageAvailable {
		t.Fatalf("expected available stage, got %s", status.Stage)
	}
	if status.Available == nil || status.Available.Version != "v0.0.2" {
		t.Fatalf("unexpected available status: %+v", status.Available)
	}
}

func TestApplyRemoteArtifactURL(t *testing.T) {
	tempDir := t.TempDir()
	cfg := config.Default()
	cfg.Version = "v0.0.1"
	cfg.DataDir = filepath.Join(tempDir, "companion")
	cfg.UpdateAllowSelfRestart = false

	current := cfg.UpdateBinCurrentPath()
	if err := os.MkdirAll(filepath.Dir(current), 0o755); err != nil {
		t.Fatalf("mkdir current dir: %v", err)
	}
	if err := os.WriteFile(current, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write current: %v", err)
	}

	newPayload := []byte("new-binary-from-url")
	sum := sha256.Sum256(newPayload)
	sumHex := hex.EncodeToString(sum[:])
	artifactServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(newPayload)
	}))
	defer artifactServer.Close()

	m := NewManager(cfg, log.New(&bytes.Buffer{}, "", 0), nil)
	applyStatus, err := m.Apply(&Artifact{
		Version: "v0.0.2",
		Path:    artifactServer.URL + "/companion",
		SHA256:  sumHex,
	})
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if applyStatus.Stage != StageHealthy {
		t.Fatalf("expected healthy stage, got %s", applyStatus.Stage)
	}
}

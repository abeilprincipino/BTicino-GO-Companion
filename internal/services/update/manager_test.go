package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestCheckOverrideMissingArtifact(t *testing.T) {
	cfg := config.Default()
	cfg.Version = "v1.0.0"
	m := NewManager(cfg, log.New(&bytes.Buffer{}, "", 0), nil)

	_, err := m.Check(&Artifact{Version: "v1.1.0"})
	if !errors.Is(err, ErrMissingArtifact) {
		t.Fatalf("expected ErrMissingArtifact, got %v", err)
	}
}

func TestApplyWithoutAvailableUpdate(t *testing.T) {
	cfg := config.Default()
	m := NewManager(cfg, log.New(&bytes.Buffer{}, "", 0), nil)

	_, err := m.Apply(nil)
	if !errors.Is(err, ErrNoAvailableUpdate) {
		t.Fatalf("expected ErrNoAvailableUpdate, got %v", err)
	}
}

func TestRollbackWithoutPreviousBinary(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "companion")
	m := NewManager(cfg, log.New(&bytes.Buffer{}, "", 0), nil)

	_, err := m.Rollback()
	if !errors.Is(err, ErrNoPreviousBinary) {
		t.Fatalf("expected ErrNoPreviousBinary, got %v", err)
	}
}

func TestHealthWindowCheck(t *testing.T) {
	cfg := config.Default()
	cfg.UpdateHealthTimeoutSec = 1
	called := false
	m := NewManager(cfg, log.New(&bytes.Buffer{}, "", 0), func(ctx context.Context) error {
		called = true
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected context deadline in health check")
		}
		if time.Until(deadline) <= 0 {
			t.Fatal("expected future deadline in health check")
		}
		return nil
	})
	if err := m.healthWindowCheck(); err != nil {
		t.Fatalf("healthWindowCheck failed: %v", err)
	}
	if !called {
		t.Fatal("expected health callback to be called")
	}
}

func TestUtilityHelpers(t *testing.T) {
	if got := parseChecksum("abc"); got != "" {
		t.Fatalf("expected empty checksum, got %q", got)
	}
	sum := strings.Repeat("a", 64)
	if got := parseChecksum(sum + "  companion\n"); got != sum {
		t.Fatalf("unexpected parsed checksum: %q", got)
	}

	if !isLowerHex("0123abcdef") {
		t.Fatal("expected lower hex to be valid")
	}
	if isLowerHex("ABCDEF") {
		t.Fatal("expected upper hex to be invalid")
	}

	if !isNewerVersion("v1.1.0", "v1.0.0") {
		t.Fatal("expected v1.1.0 to be newer")
	}
	if isNewerVersion("v1.0.0", "v1.0.0") {
		t.Fatal("expected equal versions to be not newer")
	}
	if got := normalizeSemver("1.2.3"); got != "v1.2.3" {
		t.Fatalf("unexpected normalized semver: %q", got)
	}
	if got := firstNonEmpty(" ", "x", "y"); got != "x" {
		t.Fatalf("unexpected firstNonEmpty result: %q", got)
	}
	if max(1, 2) != 2 || max(3, 1) != 3 {
		t.Fatal("unexpected max helper behavior")
	}
}

func TestVerifySHA256AndCopyFile(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src.bin")
	dst := filepath.Join(base, "dst.bin")
	data := []byte("payload")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	sum := sha256.Sum256(data)
	expected := hex.EncodeToString(sum[:])
	if err := verifySHA256(src, expected); err != nil {
		t.Fatalf("verifySHA256 failed: %v", err)
	}
	if err := verifySHA256(src, strings.Repeat("b", 64)); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if err := verifySHA256(src, "bad"); err == nil {
		t.Fatal("expected invalid checksum format error")
	}

	if err := copyFile(src, dst, 0o755); err != nil {
		t.Fatalf("copyFile failed: %v", err)
	}
	copied, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(copied) != string(data) {
		t.Fatalf("unexpected copied payload: %q", string(copied))
	}
}

func TestHTTPStatusError(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       io.NopCloser(strings.NewReader("boom")),
	}
	err := httpStatusError("download", resp)
	if err == nil || !strings.Contains(err.Error(), "status=502") {
		t.Fatalf("unexpected httpStatusError result: %v", err)
	}
}

func TestStatusReturnsCopy(t *testing.T) {
	cfg := config.Default()
	cfg.Version = "v1.0.0"
	m := NewManager(cfg, log.New(&bytes.Buffer{}, "", 0), nil)

	candidatePath := filepath.Join(t.TempDir(), "candidate.bin")
	if err := os.WriteFile(candidatePath, []byte("candidate"), 0o755); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	_, err := m.Check(&Artifact{Version: "v1.1.0", Path: candidatePath})
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}

	status := m.Status()
	if status.Available == nil {
		t.Fatalf("expected available artifact in status: %+v", status)
	}
	status.Available.Version = "tampered"
	status2 := m.Status()
	if status2.Available == nil || status2.Available.Version != "v1.1.0" {
		t.Fatalf("expected internal status copy to remain unchanged: %+v", status2)
	}
}

func TestDoGETSetsHeaders(t *testing.T) {
	cfg := config.Default()
	m := NewManager(cfg, log.New(&bytes.Buffer{}, "", 0), nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != updateUserAgent {
			t.Fatalf("unexpected user-agent: %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("unexpected accept header: %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	resp, err := m.doGET(srv.URL, "application/json")
	if err != nil {
		t.Fatalf("doGET failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected response status: %d", resp.StatusCode)
	}
}

func TestReadManifestVariants(t *testing.T) {
	cfg := config.Default()
	cfg.UpdateManifestPath = filepath.Join(t.TempDir(), "manifest.json")
	m := NewManager(cfg, log.New(&bytes.Buffer{}, "", 0), nil)

	if err := os.WriteFile(cfg.UpdateManifestPath, []byte(`{"available_version":"v1.2.0","artifact_path":"/tmp/bin","sha256":"ABC"}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	a, found, err := m.readManifest()
	if err != nil || !found {
		t.Fatalf("expected manifest found, got found=%v err=%v", found, err)
	}
	if a.SHA256 != "abc" {
		t.Fatalf("expected checksum to be normalized lowercase, got %q", a.SHA256)
	}

	if err := os.WriteFile(cfg.UpdateManifestPath, []byte(`{"available_version":"","artifact_path":"/tmp/bin"}`), 0o644); err != nil {
		t.Fatalf("write empty-version manifest: %v", err)
	}
	_, found, err = m.readManifest()
	if err != nil || found {
		t.Fatalf("expected manifest ignored when incomplete, got found=%v err=%v", found, err)
	}

	if err := os.WriteFile(cfg.UpdateManifestPath, []byte("{"), 0o644); err != nil {
		t.Fatalf("write invalid manifest: %v", err)
	}
	if _, _, err := m.readManifest(); err == nil {
		t.Fatal("expected parse error for invalid manifest JSON")
	}
}

func TestReadGitHubReleaseMissingAsset(t *testing.T) {
	cfg := config.Default()
	cfg.UpdateReleaseRepo = "owner/repo"
	cfg.UpdateReleaseAsset = "companion"

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v0.2.0",
			"assets": []map[string]any{
				{"name": "other", "browser_download_url": "https://example.invalid/other"},
			},
		})
	}))
	defer apiServer.Close()

	cfg.UpdateReleaseAPI = apiServer.URL
	m := NewManager(cfg, log.New(&bytes.Buffer{}, "", 0), nil)
	if _, _, err := m.readGitHubRelease(); err == nil {
		t.Fatal("expected missing release asset error")
	}
}

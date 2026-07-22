package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdaterStagesVerifiedNewerBinary(t *testing.T) {
	t.Parallel()

	binary := []byte("new companion")
	data := testReleaseArchive(t, companionBundlePath, binary)
	digest := sha256.Sum256(data)
	dir := t.TempDir()
	updater := NewUpdater(&testReleaseSource{manifest: ReleaseManifest{TagName: "v1.2.4", Assets: []ReleaseAsset{{Name: companionAssetName, Digest: "sha256:" + hex.EncodeToString(digest[:])}}}, data: data}, BuildInfo{Version: "v1.2.3", ReleaseRepo: "owner/repo"}, func() UpdatePolicy { return UpdatePolicy{Enabled: true, Exposed: true, DataDir: dir} }, nil)

	status, err := updater.Stage(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if !status.RestartRequired || status.StagedVersion != "v1.2.4" || status.Stage != "staged" {
		t.Fatalf("status = %#v", status)
	}

	staged, err := os.ReadFile(filepath.Join(dir, "companion.new"))
	if err != nil || !bytes.Equal(staged, binary) {
		t.Fatalf("staged = %q, err = %v", staged, err)
	}
}

func TestUpdaterRejectsDigestMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := testReleaseArchive(t, companionBundlePath, []byte("tampered"))

	updater := NewUpdater(&testReleaseSource{manifest: ReleaseManifest{TagName: "v1.2.4", Assets: []ReleaseAsset{{Name: companionAssetName, Digest: "sha256:" + strings.Repeat("0", 64)}}}, data: data}, BuildInfo{Version: "v1.2.3", ReleaseRepo: "owner/repo"}, func() UpdatePolicy { return UpdatePolicy{Enabled: true, DataDir: dir} }, nil)
	if _, err := updater.Stage(context.Background()); !errors.Is(err, ErrArtifactDigestMismatch) {
		t.Fatalf("Stage() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "companion.new")); !os.IsNotExist(err) {
		t.Fatalf("staged binary exists, err = %v", err)
	}
}

func TestUpdaterRejectsArchiveWithoutCompanionBinary(t *testing.T) {
	t.Parallel()
	data := testReleaseArchive(t, "companion/not-companion", []byte("missing"))
	digest := sha256.Sum256(data)
	dir := t.TempDir()
	updater := NewUpdater(&testReleaseSource{manifest: ReleaseManifest{TagName: "v1.2.4", Assets: []ReleaseAsset{{Name: companionAssetName, Digest: "sha256:" + hex.EncodeToString(digest[:])}}}, data: data}, BuildInfo{Version: "v1.2.3", ReleaseRepo: "owner/repo"}, func() UpdatePolicy { return UpdatePolicy{Enabled: true, DataDir: dir} }, nil)

	if _, err := updater.Stage(context.Background()); err == nil || !strings.Contains(err.Error(), "extract update archive") {
		t.Fatalf("Stage() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "companion.new")); !os.IsNotExist(err) {
		t.Fatalf("staged binary exists, err = %v", err)
	}
}

func TestUpdaterRejectsInvalidArchive(t *testing.T) {
	t.Parallel()

	data := []byte("not a tar archive")
	digest := sha256.Sum256(data)
	dir := t.TempDir()
	updater := NewUpdater(&testReleaseSource{manifest: ReleaseManifest{TagName: "v1.2.4", Assets: []ReleaseAsset{{Name: companionAssetName, Digest: "sha256:" + hex.EncodeToString(digest[:])}}}, data: data}, BuildInfo{Version: "v1.2.3", ReleaseRepo: "owner/repo"}, func() UpdatePolicy { return UpdatePolicy{Enabled: true, DataDir: dir} }, nil)

	if _, err := updater.Stage(context.Background()); err == nil || !strings.Contains(err.Error(), "extract update archive") {
		t.Fatalf("Stage() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "companion.new")); !os.IsNotExist(err) {
		t.Fatalf("staged binary exists, err = %v", err)
	}
}

func TestUpdaterDoesNotUpdateSameVersionWithDifferentSHA(t *testing.T) {
	t.Parallel()

	updater := NewUpdater(&testReleaseSource{manifest: ReleaseManifest{TagName: "v1.2.3"}}, BuildInfo{Version: "v1.2.3", GitSHA: "old", ReleaseRepo: "owner/repo"}, func() UpdatePolicy { return UpdatePolicy{Enabled: true, DataDir: t.TempDir()} }, nil)

	status, err := updater.Check(context.Background())
	if err != nil || status.UpdateAvailable {
		t.Fatalf("status = %#v, err = %v", status, err)
	}
}

func TestUpdaterUnavailableWithoutReleaseRepo(t *testing.T) {
	t.Parallel()

	updater := NewUpdater(nil, BuildInfo{Version: "v1.2.3"}, func() UpdatePolicy { return UpdatePolicy{Enabled: true} }, nil)

	status, err := updater.Status(context.Background())
	if err != nil || status.Stage != "unavailable" {
		t.Fatalf("status = %#v, err = %v", status, err)
	}
}

func TestUpdaterStatusDefaultsLatestVersionToCurrentBuild(t *testing.T) {
	t.Parallel()

	updater := NewUpdater(&testReleaseSource{}, BuildInfo{Version: "v1.2.3", ReleaseRepo: "owner/repo"}, func() UpdatePolicy {
		return UpdatePolicy{Enabled: true, DataDir: t.TempDir()}
	}, nil)

	status, err := updater.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if status.LatestVersion != "v1.2.3" || status.UpdateAvailable || status.Stage != "idle" {
		t.Fatalf("status = %#v", status)
	}
}

func TestUpdateStatusSerializesLatestVersion(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(UpdateStatus{LatestVersion: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}

	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}

	if payload["latest_version"] != "v1.2.3" || payload["latest"] != nil {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestUpdaterInstallStagesBeforeDelayedRestart(t *testing.T) {
	t.Parallel()

	binary := []byte("new companion")
	data := testReleaseArchive(t, companionBundlePath, binary)
	digest := sha256.Sum256(data)
	restarted := make(chan struct{}, 1)
	updater := NewUpdater(
		&testReleaseSource{manifest: ReleaseManifest{TagName: "v1.2.4", Assets: []ReleaseAsset{{Name: companionAssetName, Digest: "sha256:" + hex.EncodeToString(digest[:])}}}, data: data},
		BuildInfo{Version: "v1.2.3", ReleaseRepo: "owner/repo"},
		func() UpdatePolicy { return UpdatePolicy{Enabled: true, DataDir: t.TempDir()} },
		func(context.Context) error {
			restarted <- struct{}{}
			return nil
		},
	)

	status, err := updater.Install(context.Background())
	if err != nil || !status.RestartRequired {
		t.Fatalf("Install() status = %#v, err = %v", status, err)
	}

	select {
	case <-restarted:
		t.Fatal("restart occurred before Install returned")
	default:
	}

	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart was not requested")
	}
}

func TestNewerVersion(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		candidate, current string
		want               bool
	}{{"v1.2.4", "v1.2.3", true}, {"v1.2.3", "v1.2.3", false}, {"v1.2.3", "v1.2.3+other", false}, {"v1.2.3", "dev", false}, {"v1.2.3", "v1.2", false}, {"v1.2.3", "v1.2.3-rc.1", true}} {
		if got := newerVersion(test.candidate, test.current); got != test.want {
			t.Fatalf("newerVersion(%q, %q) = %t", test.candidate, test.current, got)
		}
	}
}

type testReleaseSource struct {
	manifest ReleaseManifest
	data     []byte
}

func (s *testReleaseSource) Latest(context.Context) (ReleaseManifest, error) { return s.manifest, nil }
func (s *testReleaseSource) Download(context.Context, ReleaseAsset) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

func testReleaseArchive(t *testing.T, path string, content []byte) []byte {
	t.Helper()
	root := t.TempDir()
	archivePath := filepath.Join(root, "companion.tar.gz")

	contentPath := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(contentPath), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(contentPath, content, 0o755); err != nil {
		t.Fatal(err)
	}

	if output, err := exec.Command("/bin/tar", "-C", root, "-czf", archivePath, "companion").CombinedOutput(); err != nil {
		t.Fatalf("create test archive: %v: %s", err, output)
	}

	archive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	return archive
}

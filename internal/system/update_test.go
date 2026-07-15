package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdaterStagesVerifiedNewerBinary(t *testing.T) {
	t.Parallel()
	data := []byte("new companion")
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
	if err != nil || !bytes.Equal(staged, data) {
		t.Fatalf("staged = %q, err = %v", staged, err)
	}
}

func TestUpdaterRejectsDigestMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	updater := NewUpdater(&testReleaseSource{manifest: ReleaseManifest{TagName: "v1.2.4", Assets: []ReleaseAsset{{Name: companionAssetName, Digest: "sha256:" + strings.Repeat("0", 64)}}}, data: []byte("tampered")}, BuildInfo{Version: "v1.2.3", ReleaseRepo: "owner/repo"}, func() UpdatePolicy { return UpdatePolicy{Enabled: true, DataDir: dir} }, nil)
	if _, err := updater.Stage(context.Background()); err != ErrArtifactDigestMismatch {
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

func TestUpdaterInstallStagesBeforeDelayedRestart(t *testing.T) {
	t.Parallel()
	data := []byte("new companion")
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

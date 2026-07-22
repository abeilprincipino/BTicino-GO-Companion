package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSnapshotManagerArmsPassiveCaptureAndPublishesLatest(t *testing.T) {
	image := []byte{0xff, 0xd8, 1, 2, 3, 0xff, 0xd9}
	manager := newSnapshotManager(t.TempDir(), nil, snapshotRunnerFunc(func(_ context.Context, output string) (snapshotProcess, error) {
		return &fakeSnapshotProcess{}, os.WriteFile(output, append([]byte("noise"), image...), 0o600)
	}))
	captured := make(chan struct{}, 1)

	manager.SetOnCaptured(func() { captured <- struct{}{} })

	attempt := manager.Arm("front-door")
	if attempt == nil {
		t.Fatal("Arm() = nil")
	}

	attempt.Consume(testRTPPacket(1))

	got := waitForSnapshot(t, manager, "front-door")
	if string(got) != string(image) {
		t.Fatalf("Latest() = %v, want %v", got, image)
	}

	select {
	case <-captured:
	case <-time.After(time.Second):
		t.Fatal("snapshot capture callback was not invoked")
	}

	if path, ok := manager.path("front-door"); !ok || filepath.Base(path) != "front-door.jpg" {
		t.Fatalf("path() = %q, %v", path, ok)
	}
}

func TestSnapshotManagerDoesNotPublishCancelledGeneration(t *testing.T) {
	started := make(chan struct{})
	manager := newSnapshotManager(t.TempDir(), nil, snapshotRunnerFunc(func(ctx context.Context, output string) (snapshotProcess, error) {
		close(started)

		go func() {
			<-ctx.Done()

			_ = os.WriteFile(output, []byte{0xff, 0xd8, 1, 0xff, 0xd9}, 0o600)
		}()

		return &fakeSnapshotProcess{}, nil
	}))

	attempt := manager.Arm("front-door")

	<-started
	attempt.Close()
	time.Sleep(2 * snapshotPollInterval)

	if _, err := manager.Latest("front-door"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("Latest() error = %v, want %v", err, ErrSnapshotNotFound)
	}
}

func TestFirstJPEGExtractsOneCompleteFrame(t *testing.T) {
	image, ok := firstJPEG([]byte{1, 0xff, 0xd8, 2, 0xff, 0xd9, 3, 0xff, 0xd8, 4, 0xff, 0xd9})
	if !ok || string(image) != string([]byte{0xff, 0xd8, 2, 0xff, 0xd9}) {
		t.Fatalf("firstJPEG() = %v, %v", image, ok)
	}
}

func TestSnapshotManagerRejectsUnsafeEntrypointFilename(t *testing.T) {
	manager := NewSnapshotManager(t.TempDir(), nil)
	if _, ok := manager.path("../gate1"); ok {
		t.Fatal("path() accepted traversal")
	}
}

func waitForSnapshot(t *testing.T, manager *SnapshotManager, entrypointID string) []byte {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if image, err := manager.Latest(entrypointID); err == nil {
			return image
		}

		time.Sleep(snapshotPollInterval)
	}

	t.Fatal("snapshot was not published")

	return nil
}

type snapshotRunnerFunc func(context.Context, string) (snapshotProcess, error)

func (f snapshotRunnerFunc) Start(ctx context.Context, output string) (snapshotProcess, error) {
	return f(ctx, output)
}

type fakeSnapshotProcess struct {
	once sync.Once
}

func (p *fakeSnapshotProcess) Close() error {
	p.once.Do(func() {})
	return nil
}

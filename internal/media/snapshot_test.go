package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
)

const snapshotTestPoll = 20 * time.Millisecond

var annexBStartCode = []byte{0, 0, 0, 1}

func TestSnapshotManagerArmsPassiveCaptureAndPublishesLatest(t *testing.T) {
	image := []byte{0xff, 0xd8, 1, 2, 3, 0xff, 0xd9}
	runner := &fakeSnapshotRunner{output: append([]byte("noise"), image...)}
	manager := newTestSnapshotManager(t, t.TempDir(), nil, runner)
	captured := make(chan struct{}, 1)

	manager.SetOnCaptured(func() { captured <- struct{}{} })

	attempt := manager.Arm("front-door")
	if attempt == nil {
		t.Fatal("Arm() = nil")
	}

	feedSnapshotKeyframe(attempt, 0, 0)

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

func TestSnapshotManagerFeedsTheDecoderFromTheFirstKeyframeAsAnnexB(t *testing.T) {
	runner := &fakeSnapshotRunner{output: []byte{0xff, 0xd8, 1, 0xff, 0xd9}}
	manager := newTestSnapshotManager(t, t.TempDir(), nil, runner)
	attempt := manager.Arm("front-door")

	// A frame before the keyframe cannot be decoded and must not reach the
	// decoder.
	attempt.Consume(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 1, Timestamp: 0, Marker: true}, Payload: testClipSlice})
	feedSnapshotKeyframe(attempt, 1, 3000)
	waitForSnapshot(t, manager, "front-door")

	inputs := runner.decoded()
	if len(inputs) != 1 {
		t.Fatalf("decoder received %d access units, want 1", len(inputs))
	}

	want := bytes.Join([][]byte{nil, testClipSPS, testClipPPS, testClipIDR, snapshotAccessUnitDelimiter}, annexBStartCode)
	if !bytes.Equal(inputs[0], want) {
		t.Fatalf("decoder input = %x, want %x", inputs[0], want)
	}
}

// The decoder keeps getting the frames after the keyframe until the JPEG comes
// out.
func TestSnapshotManagerKeepsFeedingFramesUntilTheDecoderMakesAPicture(t *testing.T) {
	image := []byte{0xff, 0xd8, 5, 0xff, 0xd9}
	runner := &fakeSnapshotRunner{output: image, pictureAfter: 3}
	manager := newTestSnapshotManager(t, t.TempDir(), nil, runner)
	attempt := manager.Arm("front-door")

	feedSnapshotKeyframe(attempt, 0, 0)
	for i := range 2 {
		attempt.Consume(&rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(3 + i), Timestamp: uint32(3000 * (i + 1)), Marker: true}, // #nosec G115 -- small test values.
			Payload: testClipSlice,
		})
	}

	if got := waitForSnapshot(t, manager, "front-door"); string(got) != string(image) {
		t.Fatalf("Latest() = %v, want %v", got, image)
	}

	inputs := runner.decoded()
	if len(inputs) != 3 {
		t.Fatalf("decoder received %d access units, want 3", len(inputs))
	}

	want := bytes.Join([][]byte{nil, testClipSlice, snapshotAccessUnitDelimiter}, annexBStartCode)
	if !bytes.Equal(inputs[1], want) {
		t.Fatalf("decoder input after the keyframe = %x, want %x", inputs[1], want)
	}
}

func TestSnapshotManagerTriesTheNextKeyframeWhenTheDecoderMakesNoPicture(t *testing.T) {
	image := []byte{0xff, 0xd8, 9, 0xff, 0xd9}
	runner := &fakeSnapshotRunner{outputs: [][]byte{[]byte("no picture"), image}}
	manager := newTestSnapshotManager(t, t.TempDir(), nil, runner)
	attempt := manager.Arm("front-door")

	feedSnapshotKeyframe(attempt, 0, 0)

	// The second keyframe goes to the decoder started after the first exited.
	deadline := time.Now().Add(time.Second)
	for runner.startCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(snapshotTestPoll)
	}

	feedSnapshotKeyframe(attempt, 3, 3000)

	if got := waitForSnapshot(t, manager, "front-door"); string(got) != string(image) {
		t.Fatalf("Latest() = %v, want %v", got, image)
	}

	if started := runner.startCount(); started != 2 {
		t.Fatalf("decoders started = %d, want 2", started)
	}
}

func TestSnapshotManagerDoesNotPublishCancelledGeneration(t *testing.T) {
	runner := &fakeSnapshotRunner{output: []byte{0xff, 0xd8, 1, 0xff, 0xd9}}
	manager := newTestSnapshotManager(t, t.TempDir(), nil, runner)

	attempt := manager.Arm("front-door")
	attempt.Close()
	feedSnapshotKeyframe(attempt, 0, 0)
	time.Sleep(5 * snapshotTestPoll)

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

// The decoder keeps running after its first picture: it is read without
// waiting for the output to end.
func TestReadFirstJPEGReturnsBeforeTheOutputEnds(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()

	go func() { _, _ = writer.Write([]byte{1, 0xff, 0xd8, 2, 0xff, 0xd9, 3}) }()

	image, err := readFirstJPEG(reader)
	if err != nil || !bytes.Equal(image, []byte{0xff, 0xd8, 2, 0xff, 0xd9}) {
		t.Fatalf("readFirstJPEG() = %v, %v", image, err)
	}
}

func TestReadFirstJPEGReturnsNothingWhenTheOutputEndsWithoutAPicture(t *testing.T) {
	image, err := readFirstJPEG(bytes.NewReader([]byte("no picture")))
	if err != nil || image != nil {
		t.Fatalf("readFirstJPEG() = %v, %v, want nil, nil", image, err)
	}
}

func TestSnapshotManagerRejectsUnsafeEntrypointFilename(t *testing.T) {
	manager := NewSnapshotManager(t.TempDir(), nil)
	if _, ok := manager.path("../gate1"); ok {
		t.Fatal("path() accepted traversal")
	}
}

// feedSnapshotKeyframe sends SPS, PPS and an IDR as single-NAL packets, the
// IDR closing the access unit.
func feedSnapshotKeyframe(attempt *snapshotAttempt, sequence uint16, timestamp uint32) {
	for i, nalu := range [][]byte{testClipSPS, testClipPPS, testClipIDR} {
		attempt.Consume(&rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: sequence + uint16(i), Timestamp: timestamp, Marker: i == 2}, // #nosec G115 -- small test values.
			Payload: nalu,
		})
	}
}

func waitForSnapshot(t *testing.T, manager *SnapshotManager, entrypointID string) []byte {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if image, err := manager.Latest(entrypointID); err == nil {
			return image
		}

		time.Sleep(snapshotTestPoll)
	}

	t.Fatal("snapshot was not published")

	return nil
}

// fakeSnapshotRunner starts decoders that record their input. Each answers
// with output, or with the next of outputs, once it has been fed pictureAfter
// access units (one when unset): with its JPEG, or by exiting when it holds
// none.
type fakeSnapshotRunner struct {
	mu           sync.Mutex
	output       []byte
	outputs      [][]byte
	pictureAfter int
	starts       int
	inputs       [][]byte
}

func (r *fakeSnapshotRunner) Start(context.Context) (snapshotProcess, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	output := r.output
	if r.starts < len(r.outputs) {
		output = r.outputs[r.starts]
	}

	r.starts++

	return &fakeSnapshotProcess{runner: r, output: output, pictureAfter: max(r.pictureAfter, 1), picture: make(chan []byte, 1)}, nil
}

func (r *fakeSnapshotRunner) decoded() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([][]byte(nil), r.inputs...)
}

func (r *fakeSnapshotRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.starts
}

type fakeSnapshotProcess struct {
	runner       *fakeSnapshotRunner
	output       []byte
	pictureAfter int
	writes       int
	picture      chan []byte
	once         sync.Once
}

func (p *fakeSnapshotProcess) Write(accessUnit []byte) error {
	p.runner.mu.Lock()
	p.runner.inputs = append(p.runner.inputs, bytes.Clone(accessUnit))
	p.runner.mu.Unlock()

	p.writes++
	if p.writes == p.pictureAfter {
		if image, ok := firstJPEG(p.output); ok {
			p.picture <- image
		} else {
			p.once.Do(func() { close(p.picture) })
		}
	}

	return nil
}

func (p *fakeSnapshotProcess) Picture() <-chan []byte { return p.picture }

func (p *fakeSnapshotProcess) Err() error { return nil }

func (p *fakeSnapshotProcess) Close() error { return nil }

func waitForRingSnapshot(t *testing.T, manager *SnapshotManager, entrypointID string) []byte {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if image, err := manager.LatestRing(entrypointID); err == nil {
			return image
		}

		time.Sleep(snapshotTestPoll)
	}

	t.Fatal("ring snapshot was not published")

	return nil
}

func TestSnapshotManagerWritesTheRingFileOnlyForARingCapture(t *testing.T) {
	image := []byte{0xff, 0xd8, 7, 0xff, 0xd9}
	manager := newTestSnapshotManager(t, t.TempDir(), nil, &fakeSnapshotRunner{output: image})

	if latest, ring := manager.UpdatedAt("front-door"); !latest.IsZero() || !ring.IsZero() {
		t.Fatalf("UpdatedAt() before any capture = %v, %v, want zero", latest, ring)
	}

	feedSnapshotKeyframe(manager.Arm("front-door"), 0, 0)
	waitForSnapshot(t, manager, "front-door")

	if _, err := manager.LatestRing("front-door"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("LatestRing() after a live-view capture error = %v, want ErrSnapshotNotFound", err)
	}

	attempt := manager.Arm("front-door")
	attempt.MarkRing()
	feedSnapshotKeyframe(attempt, 0, 0)

	if got := waitForRingSnapshot(t, manager, "front-door"); string(got) != string(image) {
		t.Fatalf("LatestRing() = %v, want %v", got, image)
	}

	if latest, ring := manager.UpdatedAt("front-door"); latest.IsZero() || ring.IsZero() {
		t.Fatalf("UpdatedAt() after a ring capture = %v, %v, want both set", latest, ring)
	}

	if path, ok := manager.ringPath("front-door"); !ok || filepath.Base(filepath.Dir(path)) != "ring" {
		t.Fatalf("ringPath() = %q, %v, want a file under snapshots/ring", path, ok)
	}
}

// The picture is announced and served from memory without waiting for the
// disk, which on the intercom is mounted sync: a failing write takes nothing
// away from it.
func TestSnapshotManagerServesThePictureWithoutTheDisk(t *testing.T) {
	image := []byte{0xff, 0xd8, 3, 0xff, 0xd9}
	notADirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	manager := newTestSnapshotManager(t, notADirectory, nil, &fakeSnapshotRunner{output: image})
	captured := make(chan struct{}, 1)

	manager.SetOnCaptured(func() { captured <- struct{}{} })

	attempt := manager.Arm("front-door")
	attempt.MarkRing()
	feedSnapshotKeyframe(attempt, 0, 0)

	select {
	case <-captured:
	case <-time.After(time.Second):
		t.Fatal("snapshot capture callback was not invoked")
	}

	if got, err := manager.LatestRing("front-door"); err != nil || !bytes.Equal(got, image) {
		t.Fatalf("LatestRing() = %v, %v, want %v", got, err, image)
	}

	if latest, ring := manager.UpdatedAt("front-door"); latest.IsZero() || !ring.Equal(latest) {
		t.Fatalf("UpdatedAt() = %v, %v, want the same capture time", latest, ring)
	}
}

// The files carry the capture time, so after a restart Home Assistant reads
// the same ring_snapshot_updated_at and sees no new ring.
func TestSnapshotManagerDatesTheFilesWithTheCaptureTime(t *testing.T) {
	image := []byte{0xff, 0xd8, 4, 0xff, 0xd9}
	dir := t.TempDir()
	manager := newTestSnapshotManager(t, dir, nil, &fakeSnapshotRunner{output: image})

	attempt := manager.Arm("front-door")
	attempt.MarkRing()
	feedSnapshotKeyframe(attempt, 0, 0)
	waitForRingSnapshot(t, manager, "front-door")

	latest, ring := manager.UpdatedAt("front-door")
	restarted := newTestSnapshotManager(t, dir, nil, &fakeSnapshotRunner{})

	deadline := time.Now().Add(2 * time.Second)
	for {
		restartedLatest, restartedRing := restarted.UpdatedAt("front-door")
		if restartedLatest.Equal(latest) && restartedRing.Equal(ring) {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("UpdatedAt() after a restart = %v, %v, want %v, %v", restartedLatest, restartedRing, latest, ring)
		}

		time.Sleep(snapshotTestPoll)
	}

	if got, err := restarted.LatestRing("front-door"); err != nil || !bytes.Equal(got, image) {
		t.Fatalf("LatestRing() after a restart = %v, %v, want %v", got, err, image)
	}
}

// The GStreamer decoder gets the keyframe and then the end of its input, which
// makes it drain the picture without waiting for more frames.
func TestGStreamerSnapshotProcessEndsItsInputAfterTheKeyframe(t *testing.T) {
	reader, writer := io.Pipe()
	process := &gstreamerSnapshotProcess{stdin: writer}

	received := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		received <- data
	}()

	if err := process.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	select {
	case data := <-received:
		if !bytes.Equal(data, []byte{1, 2, 3}) {
			t.Fatalf("decoder input = %v, want the keyframe alone", data)
		}
	case <-time.After(time.Second):
		t.Fatal("the decoder input did not end after the keyframe")
	}

	if err := process.Write([]byte{4}); !errors.Is(err, errSnapshotInputEnded) {
		t.Fatalf("second Write() error = %v, want %v", err, errSnapshotInputEnded)
	}
}

// Once the picture is taken the source keeps delivering the stream: nothing is
// copied or queued for a capture that is over.
func TestSnapshotAttemptStopsQueueingAfterTheCapture(t *testing.T) {
	runner := &fakeSnapshotRunner{output: []byte{0xff, 0xd8, 1, 0xff, 0xd9}}
	manager := newTestSnapshotManager(t, t.TempDir(), nil, runner)
	attempt := manager.Arm("front-door")

	feedSnapshotKeyframe(attempt, 0, 0)
	waitForSnapshot(t, manager, "front-door")

	deadline := time.Now().Add(time.Second)
	for !attempt.done.Load() && time.Now().Before(deadline) {
		time.Sleep(snapshotTestPoll)
	}

	for i := range 10 {
		attempt.Consume(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: uint16(10 + i)}, Payload: testClipSlice}) // #nosec G115 -- small test values.
	}

	if queued := len(attempt.packets); queued != 0 {
		t.Fatalf("%d packets queued after the capture ended", queued)
	}
}

// newTestSnapshotManager waits out the manager's background writes before the
// test's directories are removed.
func newTestSnapshotManager(t *testing.T, dir string, logger *slog.Logger, runner snapshotRunner) *SnapshotManager {
	t.Helper()

	manager := newSnapshotManager(dir, logger, runner)
	t.Cleanup(manager.writes.Wait)

	return manager
}

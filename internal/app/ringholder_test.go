package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"

	"github.com/pion/rtp"
)

type fakeHolderCoordinator struct {
	acquired chan config.Entrypoint
	events   chan media.SourceEvents
	left     chan *media.StreamLease
	gate     chan struct{}
	err      error
}

func newFakeHolderCoordinator() *fakeHolderCoordinator {
	return &fakeHolderCoordinator{
		acquired: make(chan config.Entrypoint, 4),
		events:   make(chan media.SourceEvents, 4),
		left:     make(chan *media.StreamLease, 4),
	}
}

func (f *fakeHolderCoordinator) AcquireRingingPreview(_, _ context.Context, entrypoint config.Entrypoint, events media.SourceEvents) (*media.StreamLease, error) {
	if f.gate != nil {
		<-f.gate
	}

	f.events <- events
	f.acquired <- entrypoint

	if f.err != nil {
		return nil, f.err
	}

	return &media.StreamLease{}, nil
}

func (f *fakeHolderCoordinator) Leave(lease *media.StreamLease) bool {
	f.left <- lease
	return true
}

type fakeRingClip struct {
	packets   atomic.Int32
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *fakeRingClip) VideoRTP(*rtp.Packet) { c.packets.Add(1) }
func (c *fakeRingClip) Close()               { c.closeOnce.Do(func() { close(c.closed) }) }

type fakeClipStarter struct {
	started chan *fakeRingClip
}

func newFakeClipStarter() *fakeClipStarter {
	return &fakeClipStarter{started: make(chan *fakeRingClip, 4)}
}

func (s *fakeClipStarter) start(string) ringClip {
	clip := &fakeRingClip{closed: make(chan struct{})}
	s.started <- clip

	return clip
}

var holderEntrypoints = []config.Entrypoint{
	{ID: "main", Capabilities: config.Capabilities{Stream: true}},
	{ID: "gate", Capabilities: config.Capabilities{Unlock: true}},
}

func newTestRingHolder(coordinator *fakeHolderCoordinator) *ringHolder {
	return newTestRingHolderWithClips(coordinator, newFakeClipStarter())
}

func newTestRingHolderWithClips(coordinator *fakeHolderCoordinator, clips *fakeClipStarter) *ringHolder {
	return newRingHolder(coordinator, clips.start, func() []config.Entrypoint { return holderEntrypoints }, slog.New(slog.DiscardHandler))
}

func waitClip(t *testing.T, clips *fakeClipStarter) *fakeRingClip {
	t.Helper()

	select {
	case clip := <-clips.started:
		return clip
	case <-time.After(2 * time.Second):
		t.Fatal("the ring holder never started a clip")
		return nil
	}
}

func waitClipClosed(t *testing.T, clip *fakeRingClip) {
	t.Helper()

	select {
	case <-clip.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the ring holder never closed the clip")
	}
}

func waitLeft(t *testing.T, left <-chan *media.StreamLease) *media.StreamLease {
	t.Helper()

	select {
	case lease := <-left:
		return lease
	case <-time.After(2 * time.Second):
		t.Fatal("the ring holder never left the stream")
		return nil
	}
}

func TestRingHolderHoldsTheStreamUntilTheAnswer(t *testing.T) {
	coordinator := newFakeHolderCoordinator()

	var applied []core.Event

	route := newTestRingHolder(coordinator).route(func(event core.Event) { applied = append(applied, event) })

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})

	select {
	case entrypoint := <-coordinator.acquired:
		if entrypoint.ID != "main" {
			t.Fatalf("acquired entrypoint %q, want main", entrypoint.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the ring holder never acquired the stream")
	}

	// The acquire runs on its own goroutine: give it time to store the lease.
	time.Sleep(20 * time.Millisecond)

	route(core.CallAnswered{DialogID: "d"})

	if waitLeft(t, coordinator.left) == nil {
		t.Fatal("left a nil lease")
	}

	if len(applied) != 2 {
		t.Fatalf("applied %d events, want 2 — every event still reaches the projector", len(applied))
	}
}

func TestRingHolderLeavesAStreamThatStartsAfterTheRingEnded(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	coordinator.gate = make(chan struct{})
	route := newTestRingHolder(coordinator).route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})
	route(core.IncomingCallEnded{DialogID: "d", Reason: core.CallEndReasonCancelled})

	close(coordinator.gate)

	if waitLeft(t, coordinator.left) == nil {
		t.Fatal("left a nil lease")
	}
}

func TestRingHolderIgnoresAnEntrypointWithoutStream(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	route := newTestRingHolder(coordinator).route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "gate"})

	select {
	case <-coordinator.acquired:
		t.Fatal("acquired a stream for an entrypoint that cannot stream")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRingHolderSurvivesAFailedAcquire(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	coordinator.err = errors.New("busy")
	route := newTestRingHolder(coordinator).route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})
	<-coordinator.acquired
	time.Sleep(20 * time.Millisecond)

	route(core.CallHungUp{DialogID: "d"})

	select {
	case <-coordinator.left:
		t.Fatal("left a stream it never held")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRingHolderRecordsTheRingUntilTheAnswer(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	clips := newFakeClipStarter()
	route := newTestRingHolderWithClips(coordinator, clips).route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})

	clip := waitClip(t, clips)
	events := <-coordinator.events
	<-coordinator.acquired

	if events.VideoRTP == nil {
		t.Fatal("the holder's stream events carry no video callback")
	}

	events.VideoRTP(&rtp.Packet{})

	if clip.packets.Load() != 1 {
		t.Fatalf("clip got %d packets, want 1", clip.packets.Load())
	}

	time.Sleep(20 * time.Millisecond)
	route(core.CallAnswered{DialogID: "d"})

	waitClipClosed(t, clip)
}

func TestRingHolderClosesTheClipOnAStreamFailure(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	clips := newFakeClipStarter()
	route := newTestRingHolderWithClips(coordinator, clips).route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})

	clip := waitClip(t, clips)
	events := <-coordinator.events
	<-coordinator.acquired

	events.Failed(errors.New("stall"))

	waitClipClosed(t, clip)
}

func TestRingHolderClosesTheClipWhenTheAcquireFails(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	coordinator.err = errors.New("busy")
	clips := newFakeClipStarter()
	route := newTestRingHolderWithClips(coordinator, clips).route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})

	waitClipClosed(t, waitClip(t, clips))
}

func TestRingHolderClosesTheClipOfALateAcquire(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	coordinator.gate = make(chan struct{})
	clips := newFakeClipStarter()
	route := newTestRingHolderWithClips(coordinator, clips).route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})

	clip := waitClip(t, clips)

	route(core.IncomingCallEnded{DialogID: "d", Reason: core.CallEndReasonCancelled})
	close(coordinator.gate)

	waitClipClosed(t, clip)
	waitLeft(t, coordinator.left)
}

func TestRingHolderClosesThePreviousClipOnANewRing(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	clips := newFakeClipStarter()
	route := newTestRingHolderWithClips(coordinator, clips).route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d1", EntrypointID: "main"})

	first := waitClip(t, clips)
	<-coordinator.events
	<-coordinator.acquired
	time.Sleep(20 * time.Millisecond)

	route(core.IncomingCallStarted{DialogID: "d2", EntrypointID: "main"})

	waitClipClosed(t, first)
}

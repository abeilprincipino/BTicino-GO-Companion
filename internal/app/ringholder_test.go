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

	mu        sync.Mutex
	retargets []string
	// unreserved makes RetargetStream find no stream to move, as before
	// AcquireRingingPreview reserves one.
	unreserved bool
}

func (f *fakeHolderCoordinator) RetargetStream(from string, to config.Entrypoint) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.unreserved {
		return false
	}

	f.retargets = append(f.retargets, from+">"+to.ID)

	return true
}

func (f *fakeHolderCoordinator) setUnreserved(unreserved bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.unreserved = unreserved
}

func (f *fakeHolderCoordinator) streamMoves() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.retargets...)
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
	target    atomic.Value
}

func (c *fakeRingClip) VideoRTP(*rtp.Packet)         { c.packets.Add(1) }
func (c *fakeRingClip) Retarget(entrypointID string) { c.target.Store(entrypointID) }
func (c *fakeRingClip) Close()                       { c.closeOnce.Do(func() { close(c.closed) }) }

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
	{ID: "side", DevAddr: "213", Capabilities: config.Capabilities{Stream: true, Ring: true}},
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

func waitAcquired(t *testing.T, coordinator *fakeHolderCoordinator) {
	t.Helper()

	select {
	case <-coordinator.acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the ring holder never acquired the stream")
	}
}

// waitStreamMoves waits for the stream moves to be want.
func waitStreamMoves(t *testing.T, coordinator *fakeHolderCoordinator, want ...string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := coordinator.streamMoves(); len(got) == len(want) {
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("stream moves = %v, want %v", got, want)
				}
			}

			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("stream moves = %v, want %v", coordinator.streamMoves(), want)
}

func TestRingHolderMovesTheHeldStreamToTheEntrypointThatRang(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	clips := newFakeClipStarter()
	holder := newTestRingHolderWithClips(coordinator, clips)

	var applied []core.Event

	route := holder.route(func(event core.Event) { applied = append(applied, event) })

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})
	waitAcquired(t, coordinator)
	clip := waitClip(t, clips)

	// Wait for the holder to keep the lease, which acquire does after the
	// coordinator returns.
	deadline := time.Now().Add(2 * time.Second)
	for {
		holder.mu.Lock()
		held := holder.lease != nil
		holder.mu.Unlock()

		if held || time.Now().After(deadline) {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	route(core.RingRetargeted{From: "gate", To: "side"})

	if moves := coordinator.streamMoves(); len(moves) != 0 {
		t.Fatalf("stream moves = %v after another entrypoint's move, want none", moves)
	}

	route(core.RingRetargeted{From: "main", To: "side"})
	waitStreamMoves(t, coordinator, "main>side")

	if target, _ := clip.target.Load().(string); target != "side" {
		t.Fatalf("clip target = %q, want side", target)
	}

	if label := holder.ringLabel("main"); label != "side" {
		t.Fatalf("ringLabel(main) = %q, want side", label)
	}

	if label := holder.ringLabel("gate"); label != "gate" {
		t.Fatalf("ringLabel(gate) = %q, want gate: not the ring's stream", label)
	}

	if len(applied) != 3 {
		t.Fatalf("applied %d events, want 3: every event still reaches the projector", len(applied))
	}

	// The next ring starts with no move.
	route(core.CallHungUp{DialogID: "d"})
	waitLeft(t, coordinator.left)
	route(core.IncomingCallStarted{DialogID: "d2", EntrypointID: "main"})

	if label := holder.ringLabel("main"); label != "main" {
		t.Fatalf("ringLabel(main) on the next ring = %q, want main", label)
	}
}

// The coordinator reserves the stream as the acquire starts, long before it is
// up. A card of the entrypoint that rang joins a stream labelled with its own
// entrypoint and is refused one labelled with another, so the stream must move
// at once, not when the holder has it.
func TestRingHolderMovesAStreamThatIsStillStarting(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	coordinator.gate = make(chan struct{})
	clips := newFakeClipStarter()
	holder := newTestRingHolderWithClips(coordinator, clips)
	route := holder.route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})
	clip := waitClip(t, clips)
	route(core.RingRetargeted{From: "main", To: "side"})

	if moves := coordinator.streamMoves(); len(moves) != 1 || moves[0] != "main>side" {
		t.Fatalf("stream moves = %v while the stream starts, want [main>side]", moves)
	}

	if target, _ := clip.target.Load().(string); target != "" {
		t.Fatalf("clip target = %q before the holder has the clip, want none", target)
	}

	close(coordinator.gate)
	waitAcquired(t, coordinator)
	waitClipTarget(t, clip, "side")

	if moves := coordinator.streamMoves(); len(moves) != 1 {
		t.Fatalf("stream moves = %v once the stream is up, want the one move", moves)
	}
}

// A move that comes before the acquire reserved the stream finds nothing to
// relabel: the holder moves the stream once it has it.
func TestRingHolderMovesAStreamReservedAfterTheMove(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	coordinator.gate = make(chan struct{})
	coordinator.setUnreserved(true)
	clips := newFakeClipStarter()
	holder := newTestRingHolderWithClips(coordinator, clips)
	route := holder.route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})
	clip := waitClip(t, clips)
	route(core.RingRetargeted{From: "main", To: "side"})

	if moves := coordinator.streamMoves(); len(moves) != 0 {
		t.Fatalf("stream moves = %v before the stream is reserved, want none", moves)
	}

	coordinator.setUnreserved(false)
	close(coordinator.gate)
	waitAcquired(t, coordinator)
	waitStreamMoves(t, coordinator, "main>side")
	waitClipTarget(t, clip, "side")
}

func waitClipTarget(t *testing.T, clip *fakeRingClip, want string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if target, _ := clip.target.Load().(string); target == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("clip target = %q, want %q", target, want)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

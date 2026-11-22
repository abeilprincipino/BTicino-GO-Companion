package app

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"bticino-go-companion/internal/core"
)

type fakeAttributionLearner struct {
	holds    map[string]time.Duration
	observed []string
}

func (l *fakeAttributionLearner) Hold(address string) time.Duration { return l.holds[address] }

func (l *fakeAttributionLearner) Observe(address, entrypointID string, delay time.Duration) {
	l.observed = append(l.observed, address+">"+entrypointID+"@"+delay.String())
}

type publication struct {
	state  core.State
	events []core.EventType
}

type recordingPublisher struct {
	mu        sync.Mutex
	published []publication
}

func (r *recordingPublisher) publish(state core.State, events []core.EventType) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.published = append(r.published, publication{state: state, events: append([]core.EventType(nil), events...)})
}

func (r *recordingPublisher) snapshot() []publication {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]publication(nil), r.published...)
}

func newTestHeldPublisher(recorder *recordingPublisher) *heldPublisher {
	return newHeldPublisher(core.NewProjector(), recorder.publish, slog.New(slog.DiscardHandler))
}

// The sequence is that of a portal6 ring on a C100X on 2026-11-20, whose ring
// frame carried the address of portal0: the ring, the inbound call given to
// portal0, the move, and the identity frame.
func TestRingAttributionReportsAHeldRingOnceSettled(t *testing.T) {
	recorder := &recordingPublisher{}
	publisher := newTestHeldPublisher(recorder)
	learner := &fakeAttributionLearner{holds: map[string]time.Duration{"21": 761 * time.Millisecond}}
	route := routeRingAttribution(learner, publisher, slog.New(slog.DiscardHandler), publisher.apply)

	route(core.RingStarted{EntrypointID: "portal0", FrameAddress: "21"})
	publisher.apply(core.IncomingCallStarted{DialogID: "d", EntrypointID: "portal0"})
	route(core.RingRetargeted{From: "portal0", To: "portal6"})

	if published := recorder.snapshot(); len(published) != 0 {
		t.Fatalf("published %d times while held, want none", len(published))
	}

	route(core.RingIdentified{EntrypointID: "portal6", Provisional: "portal0", FrameAddress: "21", Delay: 330 * time.Millisecond})

	published := recorder.snapshot()
	if len(published) != 1 {
		t.Fatalf("published %d times, want once", len(published))
	}

	state := published[0].state
	if state.PhysicalRing == nil || state.PhysicalRing.EntrypointID != "portal6" || state.IncomingCall == nil || state.IncomingCall.EntrypointID != "portal6" {
		t.Fatalf("published state = %+v, want the ring and the call on portal6", state)
	}

	want := []core.EventType{core.EventRingStarted, core.EventIncomingCallStarted, core.EventRingRetargeted}
	if len(published[0].events) != len(want) {
		t.Fatalf("published events = %v, want %v", published[0].events, want)
	}

	for i := range want {
		if published[0].events[i] != want[i] {
			t.Fatalf("published events = %v, want %v", published[0].events, want)
		}
	}

	if len(learner.observed) != 1 || learner.observed[0] != "21>portal6@330ms" {
		t.Fatalf("observed = %v, want the correction learned", learner.observed)
	}

	// Later events are published at once.
	publisher.apply(core.RingCleared{EntrypointID: "portal6"})

	if published := recorder.snapshot(); len(published) != 2 {
		t.Fatalf("published %d times after the ring, want 2", len(published))
	}
}

func TestRingAttributionReportsTheRingWhenTheHoldEnds(t *testing.T) {
	recorder := &recordingPublisher{}
	publisher := newTestHeldPublisher(recorder)
	learner := &fakeAttributionLearner{holds: map[string]time.Duration{"21": 50 * time.Millisecond}}
	route := routeRingAttribution(learner, publisher, slog.New(slog.DiscardHandler), publisher.apply)

	route(core.RingStarted{EntrypointID: "portal0", FrameAddress: "21"})

	deadline := time.Now().Add(2 * time.Second)
	for len(recorder.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	published := recorder.snapshot()
	if len(published) != 1 || published[0].state.PhysicalRing == nil || published[0].state.PhysicalRing.EntrypointID != "portal0" {
		t.Fatalf("published = %+v, want the ring on portal0 once the hold ended", published)
	}

	// The identity frame comes late: the ring moves, and the correction is
	// learned.
	route(core.RingRetargeted{From: "portal0", To: "portal6"})
	route(core.RingIdentified{EntrypointID: "portal6", Provisional: "portal0", FrameAddress: "21", Delay: 1200 * time.Millisecond})

	if published := recorder.snapshot(); len(published) != 2 || published[1].state.PhysicalRing.EntrypointID != "portal6" {
		t.Fatalf("published = %+v, want the late move published at once", published)
	}

	if len(learner.observed) != 1 {
		t.Fatalf("observed = %v, want the late correction learned", learner.observed)
	}
}

func TestRingAttributionHoldsNothingItHasNotLearned(t *testing.T) {
	recorder := &recordingPublisher{}
	publisher := newTestHeldPublisher(recorder)
	learner := &fakeAttributionLearner{}
	route := routeRingAttribution(learner, publisher, slog.New(slog.DiscardHandler), publisher.apply)

	route(core.RingStarted{EntrypointID: "portal6", FrameAddress: "213"})

	if published := recorder.snapshot(); len(published) != 1 {
		t.Fatalf("published %d times, want the ring at once", len(published))
	}

	// A confirming identity frame is not a correction.
	route(core.RingIdentified{EntrypointID: "portal6", Provisional: "portal6", FrameAddress: "213", Delay: 350 * time.Millisecond})

	if len(learner.observed) != 0 {
		t.Fatalf("observed = %v, want nothing", learner.observed)
	}

	if published := recorder.snapshot(); len(published) != 1 {
		t.Fatalf("published %d times, want RingIdentified to publish nothing", len(published))
	}
}

func TestHeldPublisherIgnoresTheTimerOfAnEndedHold(t *testing.T) {
	recorder := &recordingPublisher{}
	publisher := newTestHeldPublisher(recorder)

	publisher.hold(30 * time.Millisecond)
	publisher.release("identified")
	publisher.hold(time.Hour)
	time.Sleep(80 * time.Millisecond)

	publisher.mu.Lock()
	held := publisher.held
	publisher.mu.Unlock()

	if !held {
		t.Fatal("the first hold's timer ended the second hold")
	}

	publisher.release("test")
}

package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
)

var repeatRingEntrypoints = []config.Entrypoint{
	{ID: "portal0", DevAddr: "21", IgnoreRepeatRing: true, Capabilities: config.Capabilities{Stream: true, Unlock: true, Ring: true}},
	{ID: "portal6", DevAddr: "213", Capabilities: config.Capabilities{Stream: true, Unlock: true, Ring: true}},
}

type repeatRingHarness struct {
	t         *testing.T
	recorder  *recordingPublisher
	publisher *heldPublisher
	guard     *repeatRingGuard
	calls     func(core.Event)
	bus       func(core.Event)

	mu       sync.Mutex
	hangups  int
	hangupFn func()
	err      error
}

func newRepeatRingHarness(t *testing.T) *repeatRingHarness {
	t.Helper()

	h := &repeatRingHarness{t: t, recorder: &recordingPublisher{}}
	h.publisher = newTestHeldPublisher(h.recorder)
	h.guard = newRepeatRingGuard(func() []config.Entrypoint { return repeatRingEntrypoints }, h.publisher, h.hangup, slog.New(slog.DiscardHandler))
	h.calls = h.guard.routeCalls(h.publisher.apply)
	h.bus = h.guard.routeBus(h.publisher.apply)

	return h
}

func (h *repeatRingHarness) hangup(context.Context) error {
	h.mu.Lock()
	h.hangups++
	fn, err := h.hangupFn, h.err
	h.mu.Unlock()

	if fn != nil {
		fn()
	}

	return err
}

func (h *repeatRingHarness) hangupCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.hangups
}

// ring plays a portal0 ring as the bus and the SIP manager report it.
func (h *repeatRingHarness) ring(dialog core.DialogID) {
	h.guard.observeFrame("*8*1#1#4#21*110##")
	h.bus(core.RingStarted{EntrypointID: "portal0", FrameAddress: "21"})
	h.calls(core.IncomingCallStarted{DialogID: dialog, EntrypointID: "portal0"})
	h.guard.observeFrame("*8*9#1#4*21##")
}

// panelEnd plays the panel ending its own call, as on 2026-11-22 at 17:08:16.
func (h *repeatRingHarness) panelEnd(dialog core.DialogID) {
	h.guard.observeFrame("*8*3#1#3*421##")
	h.guard.observeFrame("*8*3#1#3*421##")
	h.calls(core.IncomingCallEnded{DialogID: dialog, Reason: core.CallEndReasonCancelled})
	h.bus(core.RingCleared{EntrypointID: "portal0"})
}

func (h *repeatRingHarness) events() []core.EventType {
	var events []core.EventType

	for _, publication := range h.recorder.snapshot() {
		events = append(events, publication.events...)
	}

	return events
}

func (h *repeatRingHarness) last() core.State {
	h.t.Helper()

	published := h.recorder.snapshot()
	if len(published) == 0 {
		h.t.Fatal("nothing published")
	}

	return published[len(published)-1].state
}

func countEvent(events []core.EventType, want core.EventType) int {
	n := 0

	for _, event := range events {
		if event == want {
			n++
		}
	}

	return n
}

func waitHeldReleased(t *testing.T, publisher *heldPublisher) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for publisher.isHeld() {
		if time.Now().After(deadline) {
			t.Fatal("the report is still held")
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// Nobody opened the door: the panel's second call is the first one going on,
// and the clients never hear it end and start again.
func TestRepeatRingMergesTheRepeatedCallOfAnUnhandledRing(t *testing.T) {
	h := newRepeatRingHarness(t)

	h.ring("d1")
	before := len(h.recorder.snapshot())

	h.panelEnd("d1")

	if published := len(h.recorder.snapshot()); published != before {
		t.Fatalf("published %d times after the panel's end, want the end held", published-before)
	}

	h.ring("d2")

	if h.publisher.isHeld() {
		t.Fatal("the report is still held after the repeated call")
	}

	if h.hangupCount() != 0 {
		t.Fatalf("hangups = %d, want the call to go on", h.hangupCount())
	}

	after := h.recorder.snapshot()[before:]
	if len(after) != 1 || len(after[0].events) != 0 {
		t.Fatalf("publications after the end = %+v, want one with no event", after)
	}

	state := h.last()
	if state.CallState != core.CallStateRinging || state.IncomingCall == nil || state.IncomingCall.DialogID != "d2" {
		t.Fatalf("state = %+v, want the repeated call ringing", state)
	}
}

// The door was opened during the first call: the panel's second call is
// declined, and the clients hear the first call end once.
func TestRepeatRingDeclinesTheRepeatedCallAfterTheDoorOpened(t *testing.T) {
	h := newRepeatRingHarness(t)

	// The decline ends the call on SIP and then on the bus, as on 2026-11-22
	// at 17:08:36.
	h.hangupFn = func() {
		h.calls(core.CallDeclined{DialogID: "d2"})
		h.guard.observeFrame("*8*3#1#4*4110##")
		h.bus(core.RingCleared{EntrypointID: "portal0"})
	}

	h.ring("d1")
	h.guard.observeFrame("*8*19*21##")
	h.guard.observeFrame("*8*20*21##")
	before := len(h.recorder.snapshot())

	h.panelEnd("d1")
	h.ring("d2")
	waitHeldReleased(t, h.publisher)

	if h.hangupCount() != 1 {
		t.Fatalf("hangups = %d, want the repeated call declined", h.hangupCount())
	}

	var events []core.EventType
	for _, publication := range h.recorder.snapshot()[before:] {
		events = append(events, publication.events...)
	}

	if countEvent(events, core.EventRingStarted) != 0 || countEvent(events, core.EventIncomingCallStarted) != 0 || countEvent(events, core.EventCallDeclined) != 0 {
		t.Fatalf("events = %v, want nothing of the repeated call", events)
	}

	if countEvent(events, core.EventRingCleared) != 1 || countEvent(events, core.EventIncomingCallEnded) != 1 {
		t.Fatalf("events = %v, want the first call's end once", events)
	}

	if state := h.last(); state.CallState != core.CallStateIdle {
		t.Fatalf("state = %+v, want idle", state)
	}
}

// No second call came: the end is reported when the wait is over.
func TestRepeatRingReportsTheEndWhenThePanelDoesNotCallAgain(t *testing.T) {
	h := newRepeatRingHarness(t)

	now := time.Now()
	h.guard.now = func() time.Time { return now }

	h.ring("d1")
	h.panelEnd("d1")

	if !h.publisher.isHeld() {
		t.Fatal("the end is not held")
	}

	// The window is over: a ring now is a new visitor.
	now = now.Add(repeatRingWindow + time.Millisecond)
	h.publisher.releaseAs(holdRepeatRing, "timeout", nil)

	if events := h.events(); countEvent(events, core.EventRingCleared) != 1 || countEvent(events, core.EventIncomingCallEnded) != 1 {
		t.Fatalf("events = %v, want the end reported", events)
	}

	h.ring("d2")

	if events := h.events(); countEvent(events, core.EventIncomingCallStarted) != 2 {
		t.Fatalf("events = %v, want the new ring reported", events)
	}

	if h.hangupCount() != 0 {
		t.Fatalf("hangups = %d, want none", h.hangupCount())
	}
}

// What the guard leaves alone: a panel without ignore_repeat_ring, an answered
// call, and a call the indoor unit ended.
func TestRepeatRingLeavesOtherEndsAlone(t *testing.T) {
	cases := map[string][]string{
		"other panel":     {"*8*3#1#3*4213##"},
		"answered":        {"*8*2#1#4*110##", "*8*3#1#3*421##"},
		"indoor hangup":   {"*8*3#1#4*4110##"},
		"panel, normally": {"*8*3#1#4*421##"},
	}

	for name, frames := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRepeatRingHarness(t)
			h.ring("d1")

			for _, frame := range frames {
				h.guard.observeFrame(frame)
			}

			if h.publisher.isHeld() {
				t.Fatal("the end is held")
			}
		})
	}
}

// A decline that fails reports everything, rather than hiding a call that
// still rings.
func TestRepeatRingReportsTheRepeatedCallWhenTheDeclineFails(t *testing.T) {
	h := newRepeatRingHarness(t)
	h.err = errors.New("no call")

	h.ring("d1")
	h.guard.observeFrame("*8*19*21##")
	h.panelEnd("d1")
	h.ring("d2")
	waitHeldReleased(t, h.publisher)

	if events := h.events(); countEvent(events, core.EventIncomingCallStarted) != 2 {
		t.Fatalf("events = %v, want the repeated call reported", events)
	}
}

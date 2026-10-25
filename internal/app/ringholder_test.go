package app

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

type fakeHolderCoordinator struct {
	acquired chan config.Entrypoint
	left     chan *media.StreamLease
	gate     chan struct{}
	err      error
}

func newFakeHolderCoordinator() *fakeHolderCoordinator {
	return &fakeHolderCoordinator{acquired: make(chan config.Entrypoint, 4), left: make(chan *media.StreamLease, 4)}
}

func (f *fakeHolderCoordinator) AcquireRingingPreview(_, _ context.Context, entrypoint config.Entrypoint, _ media.SourceEvents) (*media.StreamLease, error) {
	if f.gate != nil {
		<-f.gate
	}

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

var holderEntrypoints = []config.Entrypoint{
	{ID: "main", Capabilities: config.Capabilities{Stream: true}},
	{ID: "gate", Capabilities: config.Capabilities{Unlock: true}},
}

func newTestRingHolder(coordinator *fakeHolderCoordinator) *ringHolder {
	return newRingHolder(coordinator, func() []config.Entrypoint { return holderEntrypoints }, slog.New(slog.DiscardHandler))
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

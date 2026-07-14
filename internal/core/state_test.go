package core

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestProjector_CallAndPreviewStateMachine(t *testing.T) {
	projector := NewProjector()

	assertState(t, projector.Snapshot(), State{
		CallState: CallStateIdle,
	})

	applyEvent(t, projector, RingStarted{EntrypointID: "main"})
	assertState(t, projector.Snapshot(), State{
		Revision:     1,
		CallState:    CallStateRinging,
		PhysicalRing: &PhysicalRing{EntrypointID: "main"},
	})

	applyEvent(t, projector, PreviewStarted{StreamID: "preview-1", EntrypointID: "main"})
	assertState(t, projector.Snapshot(), State{
		Revision:      2,
		CallState:     CallStateRinging,
		PhysicalRing:  &PhysicalRing{EntrypointID: "main"},
		PreviewStream: &PreviewStream{StreamID: "preview-1", EntrypointID: "main"},
	})

	applyEvent(t, projector, IncomingCallStarted{DialogID: "dialog-1", EntrypointID: "main"})
	assertState(t, projector.Snapshot(), State{
		Revision:      3,
		CallState:     CallStateRinging,
		PhysicalRing:  &PhysicalRing{EntrypointID: "main"},
		IncomingCall:  &IncomingCall{DialogID: "dialog-1", EntrypointID: "main"},
		PreviewStream: &PreviewStream{StreamID: "preview-1", EntrypointID: "main"},
	})

	applyEvent(t, projector, CallAnswered{DialogID: "dialog-1"})
	assertState(t, projector.Snapshot(), State{
		Revision:      4,
		CallState:     CallStateActive,
		PhysicalRing:  &PhysicalRing{EntrypointID: "main"},
		ActiveCall:    &ActiveCall{DialogID: "dialog-1", EntrypointID: "main"},
		PreviewStream: &PreviewStream{StreamID: "preview-1", EntrypointID: "main"},
	})

	applyEvent(t, projector, CallHungUp{DialogID: "dialog-1"})
	applyEvent(t, projector, RingCleared{EntrypointID: "main"})
	assertState(t, projector.Snapshot(), State{
		Revision:      6,
		CallState:     CallStatePreview,
		PreviewStream: &PreviewStream{StreamID: "preview-1", EntrypointID: "main"},
	})

	applyEvent(t, projector, PreviewStopped{StreamID: "preview-1"})
	assertState(t, projector.Snapshot(), State{Revision: 7, CallState: CallStateIdle})
}

func TestProjector_AnswerRequiresIncomingDialog(t *testing.T) {
	projector := NewProjector()

	_, err := projector.Apply(CallAnswered{DialogID: "dialog-1"})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("answer error = %v, want ErrInvalidTransition", err)
	}
	assertState(t, projector.Snapshot(), State{CallState: CallStateIdle})
}

func TestProjector_PreviewNeverAnswersIncomingCall(t *testing.T) {
	projector := NewProjector()
	applyEvent(t, projector, IncomingCallStarted{DialogID: "dialog-1", EntrypointID: "main"})
	applyEvent(t, projector, PreviewStarted{StreamID: "preview-1", EntrypointID: "main"})

	state := projector.Snapshot()
	if state.ActiveCall != nil {
		t.Fatalf("preview created active call: %#v", state.ActiveCall)
	}
	if state.IncomingCall == nil || state.IncomingCall.DialogID != "dialog-1" {
		t.Fatalf("incoming dialog = %#v, want dialog-1", state.IncomingCall)
	}
	if state.CallState != CallStateRinging {
		t.Fatalf("call state = %q, want %q", state.CallState, CallStateRinging)
	}
}

func TestProjector_DeclineClearsOnlyIncomingDialog(t *testing.T) {
	projector := NewProjector()
	applyEvent(t, projector, RingStarted{EntrypointID: "main"})
	applyEvent(t, projector, IncomingCallStarted{DialogID: "dialog-1", EntrypointID: "main"})
	applyEvent(t, projector, CallDeclined{DialogID: "dialog-1"})

	state := projector.Snapshot()
	if state.IncomingCall != nil {
		t.Fatalf("incoming dialog = %#v, want nil", state.IncomingCall)
	}
	if state.PhysicalRing == nil {
		t.Fatal("decline cleared the physical ring")
	}
	if state.CallState != CallStateRinging {
		t.Fatalf("call state = %q, want %q", state.CallState, CallStateRinging)
	}
}

func TestProjector_HangupRejectsIncomingDialog(t *testing.T) {
	projector := NewProjector()
	applyEvent(t, projector, IncomingCallStarted{DialogID: "dialog-1", EntrypointID: "main"})
	applyEvent(t, projector, CallHungUp{DialogID: "dialog-1"})

	assertState(t, projector.Snapshot(), State{Revision: 2, CallState: CallStateIdle})
}

func TestProjector_SnapshotDoesNotExposeInternalState(t *testing.T) {
	projector := NewProjector()
	applyEvent(t, projector, RingStarted{EntrypointID: "main"})

	snapshot := projector.Snapshot()
	snapshot.PhysicalRing.EntrypointID = "modified"

	if got := projector.Snapshot().PhysicalRing.EntrypointID; got != "main" {
		t.Fatalf("internal physical ring = %q, want main", got)
	}
}

func TestProjector_ConcurrentSnapshotsAndEvents(t *testing.T) {
	projector := NewProjector()
	const iterations = 100
	errs := make(chan error, 4*iterations*2)

	var writers sync.WaitGroup
	for range 4 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for range iterations {
				if _, err := projector.Apply(RingStarted{EntrypointID: "main"}); err != nil {
					errs <- err
					return
				}
				if _, err := projector.Apply(RingCleared{EntrypointID: "main"}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range iterations {
				_ = projector.Snapshot()
			}
		}()
	}
	writers.Wait()
	readers.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent apply: %v", err)
	}
	applyEvent(t, projector, RingCleared{EntrypointID: "main"})

	if state := projector.Snapshot(); state.CallState != CallStateIdle {
		t.Fatalf("call state = %q, want %q", state.CallState, CallStateIdle)
	}
}

func applyEvent(t *testing.T, projector *Projector, event Event) {
	t.Helper()
	if _, err := projector.Apply(event); err != nil {
		t.Fatalf("apply %s: %v", event.Type(), err)
	}
}

func assertState(t *testing.T, got State, want State) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
}

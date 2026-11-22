package core

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

const (
	testEntrypoint = "main"
	testPreviewID  = "preview-1"
	testDialogID   = "dialog-1"
)

func TestProjector_CallAndPreviewStateMachine(t *testing.T) {
	t.Parallel()

	projector := NewProjector()

	assertState(t, projector.Snapshot(), State{
		CallState: CallStateIdle,
	})

	applyEvent(t, projector, RingStarted{EntrypointID: testEntrypoint})
	assertState(t, projector.Snapshot(), State{
		Revision:     1,
		CallState:    CallStateRinging,
		PhysicalRing: &PhysicalRing{EntrypointID: testEntrypoint},
	})

	applyEvent(t, projector, PreviewStarted{StreamID: testPreviewID, EntrypointID: testEntrypoint})
	assertState(t, projector.Snapshot(), State{
		Revision:      2,
		CallState:     CallStateRinging,
		PhysicalRing:  &PhysicalRing{EntrypointID: testEntrypoint},
		PreviewStream: &PreviewStream{StreamID: testPreviewID, EntrypointID: testEntrypoint},
	})

	applyEvent(t, projector, IncomingCallStarted{DialogID: testDialogID, EntrypointID: testEntrypoint})
	assertState(t, projector.Snapshot(), State{
		Revision:      3,
		CallState:     CallStateRinging,
		PhysicalRing:  &PhysicalRing{EntrypointID: testEntrypoint},
		IncomingCall:  &IncomingCall{DialogID: testDialogID, EntrypointID: testEntrypoint},
		PreviewStream: &PreviewStream{StreamID: testPreviewID, EntrypointID: testEntrypoint},
	})

	applyEvent(t, projector, CallAnswered{DialogID: testDialogID})
	assertState(t, projector.Snapshot(), State{
		Revision:      4,
		CallState:     CallStateActive,
		ActiveCall:    &ActiveCall{DialogID: testDialogID, EntrypointID: testEntrypoint},
		PreviewStream: &PreviewStream{StreamID: testPreviewID, EntrypointID: testEntrypoint},
	})

	applyEvent(t, projector, CallHungUp{DialogID: testDialogID})
	applyEvent(t, projector, RingCleared{EntrypointID: testEntrypoint})
	assertState(t, projector.Snapshot(), State{
		Revision:      6,
		CallState:     CallStatePreview,
		PreviewStream: &PreviewStream{StreamID: testPreviewID, EntrypointID: testEntrypoint},
	})

	applyEvent(t, projector, PreviewStopped{StreamID: testPreviewID})
	assertState(t, projector.Snapshot(), State{Revision: 7, CallState: CallStateIdle})
}

func TestProjector_AnswerRequiresIncomingDialog(t *testing.T) {
	t.Parallel()

	projector := NewProjector()

	_, err := projector.Apply(CallAnswered{DialogID: testDialogID})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("answer error = %v, want ErrInvalidTransition", err)
	}

	assertState(t, projector.Snapshot(), State{CallState: CallStateIdle})
}

func TestProjector_PreviewNeverAnswersIncomingCall(t *testing.T) {
	t.Parallel()

	projector := NewProjector()
	applyEvent(t, projector, IncomingCallStarted{DialogID: testDialogID, EntrypointID: testEntrypoint})
	applyEvent(t, projector, PreviewStarted{StreamID: testPreviewID, EntrypointID: testEntrypoint})

	state := projector.Snapshot()
	if state.ActiveCall != nil {
		t.Fatalf("preview created active call: %#v", state.ActiveCall)
	}

	if state.IncomingCall == nil || state.IncomingCall.DialogID != testDialogID {
		t.Fatalf("incoming dialog = %#v, want dialog-1", state.IncomingCall)
	}

	if state.CallState != CallStateRinging {
		t.Fatalf("call state = %q, want %q", state.CallState, CallStateRinging)
	}
}

func TestProjector_DeclineClearsOnlyIncomingDialog(t *testing.T) {
	t.Parallel()

	projector := NewProjector()
	applyEvent(t, projector, RingStarted{EntrypointID: testEntrypoint})
	applyEvent(t, projector, IncomingCallStarted{DialogID: testDialogID, EntrypointID: testEntrypoint})
	applyEvent(t, projector, CallDeclined{DialogID: testDialogID})

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
	t.Parallel()

	projector := NewProjector()
	applyEvent(t, projector, IncomingCallStarted{DialogID: testDialogID, EntrypointID: testEntrypoint})
	applyEvent(t, projector, CallHungUp{DialogID: testDialogID})

	assertState(t, projector.Snapshot(), State{Revision: 2, CallState: CallStateIdle})
}

func TestProjectorVoicemailStateIsPresentOnlyAfterStatusEvent(t *testing.T) {
	t.Parallel()

	projector := NewProjector()
	if projector.Snapshot().Voicemail != nil {
		t.Fatal("voicemail state is present before the intercom confirms support")
	}

	applyEvent(t, projector, VoicemailEnabled{})

	if voicemail := projector.Snapshot().Voicemail; voicemail == nil || !voicemail.Enabled {
		t.Fatalf("voicemail = %#v, want enabled state", voicemail)
	}

	applyEvent(t, projector, VoicemailDisabled{})

	if voicemail := projector.Snapshot().Voicemail; voicemail == nil || voicemail.Enabled {
		t.Fatalf("voicemail = %#v, want disabled state", voicemail)
	}

	applyEvent(t, projector, VoicemailUnavailable{})

	if projector.Snapshot().Voicemail != nil {
		t.Fatal("voicemail state remains present after an unavailable status")
	}
}

func TestProjector_SnapshotDoesNotExposeInternalState(t *testing.T) {
	t.Parallel()

	projector := NewProjector()
	applyEvent(t, projector, RingStarted{EntrypointID: testEntrypoint})

	snapshot := projector.Snapshot()
	snapshot.PhysicalRing.EntrypointID = "modified"

	if got := projector.Snapshot().PhysicalRing.EntrypointID; got != testEntrypoint {
		t.Fatalf("internal physical ring = %q, want main", got)
	}
}

func TestProjector_ConcurrentSnapshotsAndEvents(t *testing.T) {
	t.Parallel()

	projector := NewProjector()

	const iterations = 100

	errs := make(chan error, 4*iterations*2)

	var writers sync.WaitGroup
	for range 4 {
		writers.Go(func() {
			for range iterations {
				if _, err := projector.Apply(RingStarted{EntrypointID: testEntrypoint}); err != nil {
					errs <- err
					return
				}

				if _, err := projector.Apply(RingCleared{EntrypointID: testEntrypoint}); err != nil {
					errs <- err
					return
				}
			}
		})
	}

	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for range iterations {
				_ = projector.Snapshot()
			}
		})
	}

	writers.Wait()
	readers.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent apply: %v", err)
	}

	applyEvent(t, projector, RingCleared{EntrypointID: testEntrypoint})

	if state := projector.Snapshot(); state.CallState != CallStateIdle {
		t.Fatalf("call state = %q, want %q", state.CallState, CallStateIdle)
	}
}

func TestIncomingCallEndedRecordsReason(t *testing.T) {
	t.Parallel()

	projector := NewProjector()

	if _, err := projector.Apply(IncomingCallStarted{DialogID: "d1", EntrypointID: "main"}); err != nil {
		t.Fatalf("IncomingCallStarted error = %v", err)
	}

	if _, err := projector.Apply(IncomingCallEnded{DialogID: "d1", Reason: CallEndReasonElsewhere}); err != nil {
		t.Fatalf("IncomingCallEnded error = %v", err)
	}

	state := projector.Snapshot()
	if state.IncomingCall != nil {
		t.Fatal("IncomingCall must be cleared")
	}

	if state.LastIncomingCallEnd == nil {
		t.Fatal("LastIncomingCallEnd must be recorded")
	}

	if state.LastIncomingCallEnd.Reason != CallEndReasonElsewhere {
		t.Fatalf("Reason = %q, want elsewhere", state.LastIncomingCallEnd.Reason)
	}

	if state.CallState != CallStateIdle {
		t.Fatalf("CallState = %q, want idle", state.CallState)
	}
}

func TestIncomingCallStartedClearsPreviousEndReason(t *testing.T) {
	t.Parallel()

	projector := NewProjector()

	if _, err := projector.Apply(IncomingCallStarted{DialogID: "d1", EntrypointID: "main"}); err != nil {
		t.Fatal(err)
	}

	if _, err := projector.Apply(IncomingCallEnded{DialogID: "d1", Reason: CallEndReasonElsewhere}); err != nil {
		t.Fatal(err)
	}

	if _, err := projector.Apply(IncomingCallStarted{DialogID: "d2", EntrypointID: "main"}); err != nil {
		t.Fatal(err)
	}

	if projector.Snapshot().LastIncomingCallEnd != nil {
		t.Fatal("a new incoming call must clear the previous end reason")
	}
}

func TestProjector_SnapshotDoesNotExposeLastIncomingCallEnd(t *testing.T) {
	t.Parallel()

	projector := NewProjector()
	applyEvent(t, projector, IncomingCallStarted{DialogID: "d1", EntrypointID: "main"})
	applyEvent(t, projector, IncomingCallEnded{DialogID: "d1", Reason: CallEndReasonElsewhere})

	snapshot := projector.Snapshot()
	snapshot.LastIncomingCallEnd.Reason = CallEndReasonCancelled

	if got := projector.Snapshot().LastIncomingCallEnd.Reason; got != CallEndReasonElsewhere {
		t.Fatalf("internal last incoming call end reason = %q, want elsewhere", got)
	}
}

func applyEvent(t *testing.T, projector *Projector, event Event) {
	t.Helper()

	if _, err := projector.Apply(event); err != nil {
		t.Fatalf("apply %s: %v", event.Type(), err)
	}
}

func assertState(t *testing.T, got, want State) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
}

func TestProjector_HangupAfterAnswerGoesIdleNotRinging(t *testing.T) {
	t.Parallel()

	projector := NewProjector()
	applyEvent(t, projector, RingStarted{EntrypointID: testEntrypoint})
	applyEvent(t, projector, IncomingCallStarted{DialogID: testDialogID, EntrypointID: testEntrypoint})
	applyEvent(t, projector, CallAnswered{DialogID: testDialogID})

	if ring := projector.Snapshot().PhysicalRing; ring != nil {
		t.Fatalf("physical ring after the answer = %#v, want nil", ring)
	}

	// The bus stop frame arrives only after the media stops, so the hangup is
	// applied while it is still pending.
	applyEvent(t, projector, CallHungUp{DialogID: testDialogID})

	if state := projector.Snapshot().CallState; state != CallStateIdle {
		t.Fatalf("call state after hangup = %q, want %q", state, CallStateIdle)
	}

	// The late stop frame is a no-op.
	applyEvent(t, projector, RingCleared{EntrypointID: testEntrypoint})

	if state := projector.Snapshot().CallState; state != CallStateIdle {
		t.Fatalf("call state after the stop frame = %q, want %q", state, CallStateIdle)
	}
}

func TestProjector_HangingUpARingingCallGoesIdle(t *testing.T) {
	t.Parallel()

	projector := NewProjector()
	applyEvent(t, projector, RingStarted{EntrypointID: testEntrypoint})
	applyEvent(t, projector, IncomingCallStarted{DialogID: testDialogID, EntrypointID: testEntrypoint})

	// The bus stop frame is still pending when the user refuses the call.
	applyEvent(t, projector, CallHungUp{DialogID: testDialogID})

	if state := projector.Snapshot().CallState; state != CallStateIdle {
		t.Fatalf("call state = %q, want %q", state, CallStateIdle)
	}

	applyEvent(t, projector, RingCleared{EntrypointID: testEntrypoint})

	if state := projector.Snapshot().CallState; state != CallStateIdle {
		t.Fatalf("call state after the stop frame = %q, want %q", state, CallStateIdle)
	}
}

func TestProjector_AnswerKeepsAnotherEntrypointsRing(t *testing.T) {
	t.Parallel()

	projector := NewProjector()
	applyEvent(t, projector, RingStarted{EntrypointID: "side"})
	applyEvent(t, projector, IncomingCallStarted{DialogID: testDialogID, EntrypointID: testEntrypoint})
	applyEvent(t, projector, CallAnswered{DialogID: testDialogID})

	if ring := projector.Snapshot().PhysicalRing; ring == nil || ring.EntrypointID != "side" {
		t.Fatalf("physical ring = %#v, want the side entrypoint's ring kept", ring)
	}
}

func TestProjector_RingRetargetedMovesTheRingAndWhatBelongsToIt(t *testing.T) {
	t.Parallel()

	projector := NewProjector()
	applyEvent(t, projector, RingStarted{EntrypointID: "portal0"})
	applyEvent(t, projector, IncomingCallStarted{DialogID: testDialogID, EntrypointID: "portal0"})
	applyEvent(t, projector, PreviewStarted{StreamID: testPreviewID, EntrypointID: "portal0"})
	applyEvent(t, projector, RingRetargeted{From: "portal0", To: "portal6"})

	assertState(t, projector.Snapshot(), State{
		Revision:      4,
		CallState:     CallStateRinging,
		PhysicalRing:  &PhysicalRing{EntrypointID: "portal6"},
		IncomingCall:  &IncomingCall{DialogID: testDialogID, EntrypointID: "portal6"},
		PreviewStream: &PreviewStream{StreamID: testPreviewID, EntrypointID: "portal6"},
	})

	// A second report of the same move, from the SIP side, changes nothing.
	applyEvent(t, projector, RingRetargeted{From: "portal0", To: "portal6"})

	if ring := projector.Snapshot().PhysicalRing; ring == nil || ring.EntrypointID != "portal6" {
		t.Fatalf("ring = %#v, want portal6", ring)
	}

	if _, err := projector.Apply(RingRetargeted{From: "portal6"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("retarget to nothing error = %v, want ErrInvalidTransition", err)
	}
}

func TestProjector_PreviewStartedMovesItsOwnStreamOnly(t *testing.T) {
	t.Parallel()

	projector := NewProjector()
	applyEvent(t, projector, PreviewStarted{StreamID: testPreviewID, EntrypointID: "portal0"})
	applyEvent(t, projector, PreviewStarted{StreamID: testPreviewID, EntrypointID: "portal6"})

	if preview := projector.Snapshot().PreviewStream; preview == nil || preview.EntrypointID != "portal6" {
		t.Fatalf("preview = %#v, want the same stream on portal6", preview)
	}

	if _, err := projector.Apply(PreviewStarted{StreamID: "preview-2", EntrypointID: "portal0"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second stream error = %v, want ErrInvalidTransition", err)
	}
}

func TestProjectorTracksIntercomAvailability(t *testing.T) {
	projector := NewProjector()

	if _, err := projector.Apply(IntercomUnavailable{}); err == nil {
		t.Fatal("Apply(IntercomUnavailable{}) without a reason succeeded")
	}

	if _, err := projector.Apply(IntercomUnavailable{Reason: IntercomReloading}); err != nil {
		t.Fatal(err)
	}

	if got := projector.Snapshot().IntercomUnavailable; got != IntercomReloading {
		t.Fatalf("IntercomUnavailable = %q, want %q", got, IntercomReloading)
	}

	if _, err := projector.Apply(IntercomAvailable{}); err != nil {
		t.Fatal(err)
	}

	if got := projector.Snapshot().IntercomUnavailable; got != "" {
		t.Fatalf("IntercomUnavailable = %q after IntercomAvailable, want empty", got)
	}
}

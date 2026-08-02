package signaling

import (
	"bticino-go-companion/internal/core"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const testDialogID = "dialog-1"

func testResolver(id core.EntrypointID, devAddr string) EntrypointResolver {
	return func() (core.EntrypointID, string) { return id, devAddr }
}

func TestManager_OnInviteStoresDialogRingsAndPublishes(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &syncEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatalf("OnInvite() error = %v", err)
	}

	if len(dialog.responses) != 1 || dialog.responses[0].status != 180 || dialog.responses[0].reason != "Ringing" {
		t.Fatalf("responses = %#v, want 180 Ringing", dialog.responses)
	}

	got := events.waitForEvents(t, 1)

	if event, ok := got[0].(core.IncomingCallStarted); !ok || event.DialogID != testDialogID || event.EntrypointID != "main" {
		t.Fatalf("event = %#v, want IncomingCallStarted for dialog-1/main", got[0])
	}
}

func TestManager_OnInviteRejectsUnattributableCall(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &syncEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("", ""))

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatalf("OnInvite() error = %v", err)
	}

	if len(dialog.responses) != 1 || dialog.responses[0].status != 486 {
		t.Fatalf("responses = %#v, want 486 Busy Here", dialog.responses)
	}

	events.waitForEvents(t, 0)
}

func TestManager_AnswerMovesIncomingToActiveWithSDP(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &syncEventSink{}

	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatalf("Answer() error = %v", err)
	}

	if len(dialog.responses) != 2 || dialog.responses[1].status != 200 || dialog.responses[1].reason != "OK" {
		t.Fatalf("responses = %#v, want trailing 200 OK", dialog.responses)
	}

	if !strings.Contains(dialog.responses[1].body, "a=DEVADDR:21") {
		t.Fatalf("answer SDP missing DEVADDR: %s", dialog.responses[1].body)
	}

	if !strings.Contains(dialog.responses[1].body, "m=audio 65000 RTP/SAVP 110") || !strings.Contains(dialog.responses[1].body, "m=video 65002 RTP/SAVP 96") {
		t.Fatalf("answer SDP has wrong ports: %s", dialog.responses[1].body)
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup() error = %v", err)
	}

	if dialog.byes != 1 {
		t.Fatalf("bye count = %d, want 1", dialog.byes)
	}
}

func TestManager_DeclineSends603AndClearsIncoming(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}

	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Decline(context.Background()); err != nil {
		t.Fatalf("Decline() error = %v", err)
	}

	if len(dialog.responses) != 2 || dialog.responses[1].status != 603 || dialog.responses[1].reason != "Decline" {
		t.Fatalf("responses = %#v, want trailing 603 Decline", dialog.responses)
	}

	if err := manager.Answer(context.Background()); !errors.Is(err, ErrNoIncomingDialog) {
		t.Fatalf("Answer() error = %v, want ErrNoIncomingDialog", err)
	}
}

func TestManager_HangupDeclinesIncomingDialog(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}

	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup() error = %v", err)
	}

	if len(dialog.responses) != 2 || dialog.responses[1].status != 603 || dialog.responses[1].reason != "Decline" {
		t.Fatalf("responses = %#v, want trailing 603 Decline", dialog.responses)
	}
}

func TestManager_HangupIsIdempotent(t *testing.T) {
	t.Parallel()

	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup() on idle manager error = %v, want nil", err)
	}

	dialog := &fakeIncomingDialog{id: testDialogID}
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("first Hangup() error = %v", err)
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("second Hangup() error = %v, want nil", err)
	}

	if dialog.byes != 1 {
		t.Fatalf("bye count = %d, want 1", dialog.byes)
	}
}

func TestManager_EndIncomingPublishesReason(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &syncEventSink{}

	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	manager.EndIncoming(core.CallEndReasonElsewhere)

	got := events.waitForEvents(t, 2)

	event, ok := got[1].(core.IncomingCallEnded)
	if !ok || event.DialogID != testDialogID || event.Reason != core.CallEndReasonElsewhere {
		t.Fatalf("event = %#v, want IncomingCallEnded/elsewhere", got[1])
	}

	manager.EndIncoming(core.CallEndReasonCancelled)

	// EndIncoming must be a no-op when nothing is pending.
	events.waitForEvents(t, 2)

	if err := manager.Answer(context.Background()); !errors.Is(err, ErrNoIncomingDialog) {
		t.Fatalf("Answer() error = %v, want ErrNoIncomingDialog", err)
	}
}

// The call is left unanswered so the expiry callback really runs: answering it
// first would consume the incoming dialog and stop the timer, and the test would
// still pass with the expiry deleted.
func TestManager_IncomingCallExpiryPublishesTimeout(t *testing.T) {
	t.Parallel()

	dialog := &hookIncomingDialog{id: testDialogID}
	events := &endReasonSink{ended: make(chan core.IncomingCallEnded, 1)}

	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))
	manager.SetIncomingTimeout(20 * time.Millisecond)

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	select {
	case ended := <-events.ended:
		if ended.DialogID != testDialogID || ended.Reason != core.CallEndReasonTimeout {
			t.Fatalf("event = %#v, want IncomingCallEnded/timeout", ended)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("incoming call did not expire")
	}

	if err := manager.Answer(context.Background()); !errors.Is(err, ErrNoIncomingDialog) {
		t.Fatalf("Answer() error = %v, want ErrNoIncomingDialog", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		statuses := dialog.statuses()
		if len(statuses) == 2 && statuses[1] == 480 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("statuses = %v, want trailing 480 Temporarily Unavailable", statuses)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

func TestManager_StartStreamIsOutgoingOnly(t *testing.T) {
	t.Parallel()

	dialer := &fakeDialer{}
	manager := NewManager("192.0.2.10", dialer, &syncEventSink{}, testResolver("main", "21"))

	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatalf("StartStream() error = %v", err)
	}

	if dialer.calls != 1 {
		t.Fatalf("dialer calls = %d, want 1", dialer.calls)
	}

	if !strings.Contains(dialer.offer, "a=DEVADDR:21") {
		t.Fatalf("offer missing DEVADDR: %s", dialer.offer)
	}
}

func TestManager_StartStreamRejectsIncomingWithoutAnswering(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	dialer := &fakeDialer{}

	manager := NewManager("192.0.2.10", dialer, &syncEventSink{}, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	err := manager.StartStream(context.Background(), "21")
	if !errors.Is(err, ErrIncomingDialog) {
		t.Fatalf("StartStream() error = %v, want ErrIncomingDialog", err)
	}

	if dialer.calls != 0 {
		t.Fatalf("dialer calls = %d, want 0", dialer.calls)
	}
}

func TestManager_StartStreamSkipsInviteWhileCallAnswered(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	dialer := &fakeDialer{}

	manager := NewManager("192.0.2.10", dialer, &syncEventSink{}, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatalf("StartStream() error = %v, want nil", err)
	}

	if dialer.calls != 0 {
		t.Fatalf("dialer calls = %d, want 0 — no outbound INVITE while a call is answered", dialer.calls)
	}
}

func TestManager_StartStreamRejectsActiveOutgoingDialog(t *testing.T) {
	t.Parallel()

	dialer := &fakeDialer{}

	manager := NewManager("192.0.2.10", dialer, &syncEventSink{}, testResolver("main", "21"))
	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatalf("first StartStream() error = %v", err)
	}

	if err := manager.StartStream(context.Background(), "22"); !errors.Is(err, ErrActiveDialog) {
		t.Fatalf("second StartStream() error = %v, want ErrActiveDialog", err)
	}

	if dialer.calls != 1 {
		t.Fatalf("dialer calls = %d, want 1", dialer.calls)
	}
}

func TestManager_OnInviteRejectsSecondCallWhileRinging(t *testing.T) {
	t.Parallel()

	first := &fakeIncomingDialog{id: testDialogID}
	second := &fakeIncomingDialog{id: "dialog-2"}
	events := &syncEventSink{}

	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	if err := manager.OnInvite(context.Background(), second); err != nil {
		t.Fatalf("second OnInvite() error = %v", err)
	}

	if len(second.responses) != 1 || second.responses[0].status != 486 || second.responses[0].reason != "Busy Here" {
		t.Fatalf("second dialog responses = %#v, want 486 Busy Here", second.responses)
	}

	// The rejected call never started, so it published nothing.
	events.waitForEvents(t, 1)

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatalf("Answer() error = %v, the ringing call must be untouched", err)
	}

	if len(first.responses) != 2 || first.responses[1].status != 200 {
		t.Fatalf("first dialog responses = %#v, want trailing 200 OK", first.responses)
	}
}

func TestManager_OnInviteRejectsCallWhileOutgoingStreamActive(t *testing.T) {
	t.Parallel()

	dialer := &fakeDialer{}
	events := &syncEventSink{}

	manager := NewManager("192.0.2.10", dialer, events, testResolver("main", "21"))
	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatal(err)
	}

	dialog := &fakeIncomingDialog{id: testDialogID}
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatalf("OnInvite() error = %v", err)
	}

	if len(dialog.responses) != 1 || dialog.responses[0].status != 486 || dialog.responses[0].reason != "Busy Here" {
		t.Fatalf("responses = %#v, want 486 Busy Here — an outbound dialog is up", dialog.responses)
	}

	events.waitForEvents(t, 0)

	if err := manager.Answer(context.Background()); !errors.Is(err, ErrNoIncomingDialog) {
		t.Fatalf("Answer() error = %v, want ErrNoIncomingDialog", err)
	}

	if len(dialer.dialogs) != 1 || dialer.dialogs[0].byeCount() != 0 {
		t.Fatalf("outbound dialog was disturbed by the rejected INVITE: %#v", dialer.dialogs)
	}
}

func TestManager_OnInviteReservesIncomingBeforeItRings(t *testing.T) {
	t.Parallel()

	dialer := &fakeDialer{}
	manager := NewManager("192.0.2.10", dialer, &syncEventSink{}, testResolver("main", "21"))

	var streamErr error

	dialog := &hookIncomingDialog{id: testDialogID}
	dialog.onRespond = func(status int) error {
		if status == 180 {
			streamErr = manager.StartStream(context.Background(), "21")
		}

		return nil
	}

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatalf("OnInvite() error = %v", err)
	}

	if !errors.Is(streamErr, ErrIncomingDialog) {
		t.Fatalf("StartStream() during the 180 round trip = %v, want ErrIncomingDialog", streamErr)
	}

	if dialer.calls != 0 {
		t.Fatalf("dialer calls = %d, want 0 — the incoming slot must be reserved before the 180 is sent", dialer.calls)
	}
}

func TestManager_OnInviteRollsBackReservationWhenRingingFails(t *testing.T) {
	t.Parallel()

	ringErr := errors.New("transport down")
	dialer := &fakeDialer{}
	events := &syncEventSink{}

	manager := NewManager("192.0.2.10", dialer, events, testResolver("main", "21"))
	dialog := &hookIncomingDialog{id: testDialogID, onRespond: func(status int) error {
		if status == 180 {
			return ringErr
		}

		return nil
	}}

	if err := manager.OnInvite(context.Background(), dialog); !errors.Is(err, ringErr) {
		t.Fatalf("OnInvite() error = %v, want %v", err, ringErr)
	}

	// IncomingCallStarted is published at the commit point, before the 180 goes
	// out, so the rollback has to end the call it already announced — otherwise
	// the projector keeps a call that never rang and rejects every later INVITE.
	got := events.waitForEvents(t, 2)

	if _, ok := got[0].(core.IncomingCallStarted); !ok {
		t.Fatalf("events[0] = %#v, want IncomingCallStarted", got[0])
	}

	ended, ok := got[1].(core.IncomingCallEnded)
	if !ok || ended.DialogID != testDialogID || ended.Reason != core.CallEndReasonCancelled {
		t.Fatalf("events[1] = %#v, want IncomingCallEnded/cancelled for dialog-1", got[1])
	}

	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatalf("StartStream() after the rollback error = %v, want nil", err)
	}
}

// TestManager_OnInviteSendsFinalResponseWhenRingingFails covers the other half
// of the rollback: an INVITE that never got a provisional response must still
// get a final one, or the far end waits out its whole transaction timeout.
func TestManager_OnInviteSendsFinalResponseWhenRingingFails(t *testing.T) {
	t.Parallel()

	ringErr := errors.New("transport down")

	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))
	dialog := &hookIncomingDialog{id: testDialogID, onRespond: func(status int) error {
		if status == 180 {
			return ringErr
		}

		return nil
	}}

	if err := manager.OnInvite(context.Background(), dialog); !errors.Is(err, ringErr) {
		t.Fatalf("OnInvite() error = %v, want %v", err, ringErr)
	}

	if statuses := dialog.statuses(); len(statuses) != 2 || statuses[1] != 500 {
		t.Fatalf("statuses = %v, want trailing 500 Server Error — the INVITE needs a final response", statuses)
	}
}

// TestManager_OnInvitePublishesStartBeforeRinging pins the commit point. If the
// start event is published after the 180, any publisher that slips into that
// window — a CANCEL answered elsewhere, the expiry, a Decline — reaches the
// projector before the call it ends, and the projector wedges in "ringing"
// forever, rejecting every later inbound call.
func TestManager_OnInvitePublishesStartBeforeRinging(t *testing.T) {
	t.Parallel()

	events := &syncEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	dialog := &hookIncomingDialog{id: testDialogID}
	dialog.onRespond = func(status int) error {
		if status == 180 {
			// The intercom's CANCEL — the call was answered on another handset —
			// lands while the 180 is still on the wire.
			manager.EndIncoming(core.CallEndReasonElsewhere)
		}

		return nil
	}

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatalf("OnInvite() error = %v", err)
	}

	got := events.waitForEvents(t, 2)

	if _, ok := got[0].(core.IncomingCallStarted); !ok {
		t.Fatalf("events[0] = %#v, want IncomingCallStarted — the projector must see a call start before it ends", got[0])
	}

	ended, ok := got[1].(core.IncomingCallEnded)
	if !ok || ended.Reason != core.CallEndReasonElsewhere {
		t.Fatalf("events[1] = %#v, want IncomingCallEnded/elsewhere", got[1])
	}
}

func TestManager_AnswerByesDialogWhenItLosesTheIncomingSlot(t *testing.T) {
	t.Parallel()

	dialer := &fakeDialer{}
	manager := NewManager("192.0.2.10", dialer, &syncEventSink{}, testResolver("main", "21"))

	dialog := &hookIncomingDialog{id: testDialogID}
	dialog.onRespond = func(status int) error {
		if status == 200 {
			// The expiry timer clears the incoming slot while the 200 OK is
			// still on the wire.
			manager.EndIncoming(core.CallEndReasonTimeout)
		}

		return nil
	}

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); !errors.Is(err, ErrNoIncomingDialog) {
		t.Fatalf("Answer() error = %v, want ErrNoIncomingDialog", err)
	}

	if dialog.byeCount() != 1 {
		t.Fatalf("bye count = %d, want 1 — a dialog already sent 200 OK must not survive unreferenced", dialog.byeCount())
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup() error = %v", err)
	}

	if dialog.byeCount() != 1 {
		t.Fatalf("bye count = %d, want 1 — the lost dialog must not be held as active", dialog.byeCount())
	}
}

func TestManager_RemoteDialogEndedClearsAnsweredCallWithoutBye(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	dialer := &fakeDialer{}
	events := &syncEventSink{}

	manager := NewManager("192.0.2.10", dialer, events, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	manager.RemoteDialogEnded()

	if dialog.byes != 0 {
		t.Fatalf("bye count = %d, want 0 — the far end already ended the dialog", dialog.byes)
	}

	got := events.waitForEvents(t, 3)

	if event, ok := got[2].(core.CallHungUp); !ok || event.DialogID != testDialogID {
		t.Fatalf("event = %#v, want CallHungUp for dialog-1", got[2])
	}

	// Without this the manager keeps believing a call is answered and every
	// later preview silently succeeds without sending an INVITE.
	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatalf("StartStream() after the remote BYE error = %v", err)
	}

	if dialer.calls != 1 {
		t.Fatalf("dialer calls = %d, want 1", dialer.calls)
	}

	// The outbound preview publishes nothing, so neither does its remote end.
	manager.RemoteDialogEnded()

	// A preview has no dialog the projector knows about.
	events.waitForEvents(t, 3)
}

func TestManager_RemoteDialogEndedWithNothingActiveIsANoop(t *testing.T) {
	t.Parallel()

	events := &syncEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	manager.RemoteDialogEnded()
	manager.RemoteDialogEnded()

	events.waitForEvents(t, 0)

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup() error = %v, want nil", err)
	}
}

func TestManager_PublishesAreSerializedInCommitOrder(t *testing.T) {
	t.Parallel()

	events := newGateSink()
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	dialog := &hookIncomingDialog{id: testDialogID}

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		if err := manager.OnInvite(context.Background(), dialog); err != nil {
			t.Errorf("OnInvite() error = %v", err)
		}
	}()

	<-events.entered

	// The sink is still inside IncomingCallStarted, so both transitions below
	// commit into a queue nobody is popping. They have to come back out of it in
	// the order they committed, not in whatever order the drain happens to find
	// convenient.
	manager.EndIncoming(core.CallEndReasonCancelled)

	if err := manager.OnInvite(context.Background(), &hookIncomingDialog{id: "dialog-2"}); err != nil {
		t.Fatalf("second OnInvite() error = %v", err)
	}

	close(events.release)
	wg.Wait()
	waitForCount(t, 3, events.count)

	delivered, overlap := events.delivered()
	if overlap {
		t.Fatal("two events were delivered to the sink at once")
	}

	if started, ok := delivered[0].(core.IncomingCallStarted); !ok || started.DialogID != testDialogID {
		t.Fatalf("delivered[0] = %#v, want IncomingCallStarted for dialog-1", delivered[0])
	}

	if ended, ok := delivered[1].(core.IncomingCallEnded); !ok || ended.DialogID != testDialogID {
		t.Fatalf("delivered[1] = %#v, want IncomingCallEnded for dialog-1", delivered[1])
	}

	if started, ok := delivered[2].(core.IncomingCallStarted); !ok || started.DialogID != "dialog-2" {
		t.Fatalf("delivered[2] = %#v, want IncomingCallStarted for dialog-2", delivered[2])
	}
}

func TestManager_EndIncomingNormalizesEmptyReason(t *testing.T) {
	t.Parallel()

	events := &syncEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	if err := manager.OnInvite(context.Background(), &fakeIncomingDialog{id: testDialogID}); err != nil {
		t.Fatal(err)
	}

	manager.EndIncoming("")

	got := events.waitForEvents(t, 2)

	event, ok := got[1].(core.IncomingCallEnded)
	if !ok || event.Reason != core.CallEndReasonCancelled {
		t.Fatalf("event = %#v, want IncomingCallEnded/cancelled", got[1])
	}
}

func TestManager_PreviewTeardownPublishesNothing(t *testing.T) {
	t.Parallel()

	dialer := &fakeDialer{}
	events := &syncEventSink{}

	manager := NewManager("192.0.2.10", dialer, events, testResolver("main", "21"))
	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatal(err)
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup() error = %v", err)
	}

	// An outbound preview has no dialog the projector knows about.
	events.waitForEvents(t, 0)

	if len(dialer.dialogs) != 1 || dialer.dialogs[0].byeCount() != 1 {
		t.Fatalf("preview dialog was not hung up: %#v", dialer.dialogs)
	}
}

// TestManager_ConcurrentLifecycleLeavesNoDialogUnreferenced races the four entry
// points against an expiry short enough to fire mid-transaction. Every dialog
// the manager took past the point of no return — a 200 OK on an inbound call, an
// INVITE on an outbound one — must end up hung up rather than orphaned.
func TestManager_ConcurrentLifecycleLeavesNoDialogUnreferenced(t *testing.T) {
	t.Parallel()

	const rounds = 200

	dialer := &fakeDialer{}
	manager := NewManager("192.0.2.10", dialer, &syncEventSink{}, testResolver("main", "21"))
	manager.SetIncomingTimeout(time.Millisecond)

	dialogs := make([]*hookIncomingDialog, 0, rounds)

	for round := range rounds {
		dialog := &hookIncomingDialog{id: core.DialogID(fmt.Sprintf("dialog-%d", round))}
		dialogs = append(dialogs, dialog)

		var wg sync.WaitGroup

		wg.Add(4)

		go func() { defer wg.Done(); _ = manager.OnInvite(context.Background(), dialog) }()
		go func() { defer wg.Done(); _ = manager.Answer(context.Background()) }()
		go func() { defer wg.Done(); _ = manager.StartStream(context.Background(), "21") }()
		go func() { defer wg.Done(); _ = manager.Hangup(context.Background()) }()

		wg.Wait()

		// Drain whatever survived the round before checking the invariant.
		if err := manager.Hangup(context.Background()); err != nil {
			t.Fatalf("round %d: drain Hangup() error = %v", round, err)
		}

		manager.EndIncoming(core.CallEndReasonCancelled)
	}

	for _, dialog := range dialogs {
		if dialog.sawStatus(200) && dialog.byeCount() == 0 {
			t.Fatalf("dialog %s was answered with 200 OK but never hung up", dialog.ID())
		}
	}

	if leaked := dialer.unbyedDialogs(); len(leaked) != 0 {
		t.Fatalf("outbound dialogs %v were dialled and never hung up", leaked)
	}

	// A leaked flag would make every later preview succeed without dialling.
	before := dialer.calls
	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatalf("StartStream() after the race error = %v", err)
	}

	if dialer.calls != before+1 {
		t.Fatalf("dialer calls = %d, want %d — the manager still believes a call is up", dialer.calls, before+1)
	}
}

// TestManager_ConcurrentAnswerDoesNotByeTheAnsweredCall is the double-tap on the
// card, or a retried HTTP request: both Answer calls snapshot the same ringing
// dialog and both send a 200 OK. The one that loses the promotion must not BYE
// the call the winner has just answered.
func TestManager_ConcurrentAnswerDoesNotByeTheAnsweredCall(t *testing.T) {
	t.Parallel()

	events := &syncEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	// Neither Answer is allowed back onto the state lock until both are inside
	// their 200 OK — that is what makes the loser branch run deterministically.
	var arrived sync.WaitGroup

	arrived.Add(2)

	release := make(chan struct{})

	dialog := &hookIncomingDialog{id: testDialogID}
	dialog.onRespond = func(status int) error {
		if status == 200 {
			arrived.Done()
			<-release
		}

		return nil
	}

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 2)

	for range 2 {
		go func() { errs <- manager.Answer(context.Background()) }()
	}

	go func() { arrived.Wait(); close(release) }()

	answers := make([]error, 0, 2)
	for range 2 {
		answers = append(answers, <-errs)
	}

	if dialog.byeCount() != 0 {
		t.Fatalf("bye count = %d, want 0 — the losing Answer tore down the answered call", dialog.byeCount())
	}

	for _, err := range answers {
		if err != nil {
			t.Fatalf("Answer() error = %v, want nil — a double tap must not surface an error", err)
		}
	}

	// Exactly one answered outcome reached the projector, however many taps the
	// user managed.
	answered := 0

	for _, event := range events.waitForEvents(t, 2) {
		if _, ok := event.(core.CallAnswered); ok {
			answered++
		}
	}

	if answered != 1 {
		t.Fatalf("CallAnswered count = %d, want 1", answered)
	}

	// The dialog must still be the active one: Hangup is the only thing that
	// ends it, and it ends it exactly once.
	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup() error = %v", err)
	}

	if dialog.byeCount() != 1 {
		t.Fatalf("bye count after Hangup = %d, want 1 — the answered dialog was not held as active", dialog.byeCount())
	}
}

// TestManager_StalledSinkDoesNotBlockTheStateLock covers the real sink, which
// broadcasts to every WebSocket client synchronously with a write deadline. One
// stalled browser tab must not lock out SIP signalling, or an inbound INVITE
// gets no 180 at all and times out at the intercom.
func TestManager_StalledSinkDoesNotBlockTheStateLock(t *testing.T) {
	t.Parallel()

	events := newGateSink()
	dialer := &fakeDialer{}
	manager := NewManager("192.0.2.10", dialer, events, testResolver("main", "21"))

	invited := make(chan error, 1)

	go func() {
		invited <- manager.OnInvite(context.Background(), &hookIncomingDialog{id: testDialogID})
	}()

	<-events.entered

	// A second publisher commits while the sink is stalled. It must hand its
	// event over and return instead of blocking with the state lock held.
	ended := make(chan struct{})

	go func() {
		defer close(ended)

		manager.EndIncoming(core.CallEndReasonCancelled)
	}()

	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("EndIncoming blocked behind a stalled sink")
	}

	// And the state lock is still free for signalling.
	streamed := make(chan error, 1)

	go func() { streamed <- manager.StartStream(context.Background(), "21") }()

	select {
	case err := <-streamed:
		if err != nil {
			t.Fatalf("StartStream() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StartStream blocked behind a stalled sink")
	}

	close(events.release)

	if err := <-invited; err != nil {
		t.Fatalf("OnInvite() error = %v", err)
	}

	waitForCount(t, 2, events.count)

	if _, overlap := events.delivered(); overlap {
		t.Fatal("two events were delivered to the sink at once")
	}
}

// TestManager_ReentrantSinkDoesNotDeadlock: the sink is external code and may
// call back into the Manager. Serializing deliveries with a mutex held across
// the sink turns that into a self-deadlock on a single goroutine.
func TestManager_ReentrantSinkDoesNotDeadlock(t *testing.T) {
	t.Parallel()

	manager := NewManager("192.0.2.10", &fakeDialer{}, nil, testResolver("main", "21"))
	sink := &reentrantSink{call: func() { manager.EndIncoming(core.CallEndReasonElsewhere) }}
	manager.SetEvents(sink)

	invited := make(chan error, 1)

	go func() {
		invited <- manager.OnInvite(context.Background(), &fakeIncomingDialog{id: testDialogID})
	}()

	select {
	case err := <-invited:
		if err != nil {
			t.Fatalf("OnInvite() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a sink that calls back into the manager deadlocked the publish path")
	}

	waitForCount(t, 2, sink.count)

	got := sink.snapshot()
	if _, ok := got[0].(core.IncomingCallStarted); !ok {
		t.Fatalf("events[0] = %#v, want IncomingCallStarted", got[0])
	}

	ended, ok := got[1].(core.IncomingCallEnded)
	if !ok || ended.Reason != core.CallEndReasonElsewhere {
		t.Fatalf("events[1] = %#v, want IncomingCallEnded/elsewhere", got[1])
	}
}

// TestManager_HangupEndsAnsweredInboundCall pins the deliberate asymmetry with
// StartStream: the media layer runs Hangup on every lease teardown, and that
// ends a live doorbell call on purpose — without the WebRTC session there is no
// audio path, so the visitor would be talking to nobody.
func TestManager_HangupEndsAnsweredInboundCall(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &syncEventSink{}

	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup() error = %v", err)
	}

	if dialog.byes != 1 {
		t.Fatalf("bye count = %d, want 1 — the media lease teardown ends the answered call", dialog.byes)
	}

	got := events.waitForEvents(t, 3)

	hungUp, ok := got[2].(core.CallHungUp)
	if !ok || hungUp.DialogID != testDialogID {
		t.Fatalf("events[2] = %#v, want CallHungUp for dialog-1", got[2])
	}
}

func TestManager_StartStreamWrapsDialerError(t *testing.T) {
	t.Parallel()

	dialErr := errors.New("no route to intercom")
	dialer := &fakeDialer{err: dialErr}

	manager := NewManager("192.0.2.10", dialer, &syncEventSink{}, testResolver("main", "21"))

	err := manager.StartStream(context.Background(), "21")
	if !errors.Is(err, dialErr) {
		t.Fatalf("StartStream() error = %v, want %v", err, dialErr)
	}

	if !strings.HasPrefix(err.Error(), "outgoing stream: ") {
		t.Fatalf("StartStream() error = %q, want it wrapped with an outgoing stream prefix", err)
	}
}

func TestManager_EndIncomingNormalizesUnknownReason(t *testing.T) {
	t.Parallel()

	events := &syncEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	if err := manager.OnInvite(context.Background(), &fakeIncomingDialog{id: testDialogID}); err != nil {
		t.Fatal(err)
	}

	// The core layer does not validate the reason, so an invalid non-empty one
	// must not reach it either.
	manager.EndIncoming(core.CallEndReason("bogus"))

	got := events.waitForEvents(t, 2)

	event, ok := got[1].(core.IncomingCallEnded)
	if !ok || event.Reason != core.CallEndReasonCancelled {
		t.Fatalf("event = %#v, want IncomingCallEnded/cancelled", got[1])
	}
}

// TestManager_RingingIsNotDelayedByTheSink pins the reason the drain runs on its
// own goroutine. IncomingCallStarted is published at the commit point, before the
// 180, and the production sink broadcasts to every WebSocket client in turn with
// a write deadline on each. Carrying the drain on OnInvite's goroutine would put
// that whole broadcast in front of the provisional response, and the intercom
// gives up on an INVITE it never sees ringing.
func TestManager_RingingIsNotDelayedByTheSink(t *testing.T) {
	t.Parallel()

	events := newGateSink()
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	dialog := &hookIncomingDialog{id: testDialogID}
	invited := make(chan error, 1)

	go func() { invited <- manager.OnInvite(context.Background(), dialog) }()

	// The sink is now inside IncomingCallStarted and stays there until released.
	<-events.entered

	select {
	case err := <-invited:
		if err != nil {
			t.Fatalf("OnInvite() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnInvite waited for the sink before it answered the INVITE")
	}

	if statuses := dialog.statuses(); len(statuses) != 1 || statuses[0] != 180 {
		t.Fatalf("statuses = %v, want 180 Ringing already sent while the sink is still blocked", statuses)
	}

	close(events.release)

	waitForCount(t, 1, events.count)
}

// TestManager_RollbackSendsNoFinalResponseWhenItLosesTheDialog covers the other
// side of the failed-180 rollback. If the call was answered, declined or expired
// while the 180 was failing, the dialog already carries a final response — a 500
// on top of it is a second final response on a transaction somebody else owns.
func TestManager_RollbackSendsNoFinalResponseWhenItLosesTheDialog(t *testing.T) {
	t.Parallel()

	ringErr := errors.New("transport down")
	events := &syncEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	dialog := &hookIncomingDialog{id: testDialogID}
	dialog.onRespond = func(status int) error {
		if status == 180 {
			// The intercom's CANCEL lands while the 180 is failing, so the
			// rollback that follows no longer owns the dialog.
			manager.EndIncoming(core.CallEndReasonElsewhere)

			return ringErr
		}

		return nil
	}

	if err := manager.OnInvite(context.Background(), dialog); !errors.Is(err, ringErr) {
		t.Fatalf("OnInvite() error = %v, want %v", err, ringErr)
	}

	if statuses := dialog.statuses(); len(statuses) != 1 || statuses[0] != 180 {
		t.Fatalf("statuses = %v, want the 180 alone — the rollback lost the dialog and must not answer it", statuses)
	}

	got := events.waitForEvents(t, 2)

	ended, ok := got[1].(core.IncomingCallEnded)
	if !ok || ended.Reason != core.CallEndReasonElsewhere {
		t.Fatalf("events[1] = %#v, want IncomingCallEnded/elsewhere — the rollback must not publish a second end", got[1])
	}
}

// TestManager_PanickingSinkDoesNotStrandTheDrain: the sink is external code, and
// a panic in it used to leave the drain flag set for good — every later event was
// enqueued behind a drain that no longer existed and nothing ever reached the
// projector again. Only a restart recovered it.
func TestManager_PanickingSinkDoesNotStrandTheDrain(t *testing.T) {
	t.Parallel()

	events := newPanickingSink()
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	// The recover models net/http, which recovers a panicking handler goroutine
	// and leaves the process running: that is how this stayed silent. Once the
	// drain owns its own goroutine there is nothing here to recover, and an
	// unrecovered sink panic would take the whole process down instead.
	func() {
		defer func() { _ = recover() }()

		if err := manager.OnInvite(context.Background(), &hookIncomingDialog{id: testDialogID}); err != nil {
			t.Errorf("OnInvite() error = %v", err)
		}
	}()

	manager.EndIncoming(core.CallEndReasonCancelled)

	select {
	case event := <-events.delivered:
		if _, ok := event.(core.IncomingCallEnded); !ok {
			t.Fatalf("event = %#v, want IncomingCallEnded", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event reached the sink after it panicked — the drain flag was stranded")
	}
}

type fakeIncomingDialog struct {
	id        core.DialogID
	responses []response
	byes      int
}

func (d *fakeIncomingDialog) ID() core.DialogID { return d.id }

func (d *fakeIncomingDialog) Respond(_ context.Context, status int, reason, body string) error {
	d.responses = append(d.responses, response{status: status, reason: reason, body: body})
	return nil
}

func (d *fakeIncomingDialog) Bye(context.Context) error {
	d.byes++
	return nil
}

// hookIncomingDialog is the fake used by every test where the manager touches
// the dialog from more than one goroutine — the expiry timer, or the test's own
// racers. onRespond runs inside Respond, which is how a test observes the
// manager's state at the exact point it is waiting on the network.
type hookIncomingDialog struct {
	id        core.DialogID
	onRespond func(status int) error

	mu        sync.Mutex
	responses []response
	byes      int
}

func (d *hookIncomingDialog) ID() core.DialogID { return d.id }

func (d *hookIncomingDialog) Respond(_ context.Context, status int, reason, body string) error {
	d.mu.Lock()
	d.responses = append(d.responses, response{status: status, reason: reason, body: body})
	d.mu.Unlock()

	if d.onRespond != nil {
		return d.onRespond(status)
	}

	return nil
}

func (d *hookIncomingDialog) Bye(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.byes++

	return nil
}

func (d *hookIncomingDialog) byeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.byes
}

func (d *hookIncomingDialog) statuses() []int {
	d.mu.Lock()
	defer d.mu.Unlock()

	statuses := make([]int, 0, len(d.responses))
	for _, resp := range d.responses {
		statuses = append(statuses, resp.status)
	}

	return statuses
}

func (d *hookIncomingDialog) sawStatus(status int) bool {
	for _, seen := range d.statuses() {
		if seen == status {
			return true
		}
	}

	return false
}

type endReasonSink struct {
	ended chan core.IncomingCallEnded
}

func (s *endReasonSink) Publish(event core.Event) {
	if ended, ok := event.(core.IncomingCallEnded); ok {
		s.ended <- ended
	}
}

type response struct {
	status int
	reason string
	body   string
}

type fakeDialer struct {
	mu      sync.Mutex
	err     error
	calls   int
	offer   string
	dialogs []*fakeOutgoingDialog
}

func (d *fakeDialer) StartStream(_ context.Context, _, offer string) (OutgoingDialog, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.calls++
	d.offer = offer

	if d.err != nil {
		return nil, d.err
	}

	dialog := &fakeOutgoingDialog{}
	d.dialogs = append(d.dialogs, dialog)

	return dialog, nil
}

// unbyedDialogs counts the outbound dialogs the manager dialled but never hung
// up — every one of them is a leak the far end keeps streaming into.
func (d *fakeDialer) unbyedDialogs() []int {
	d.mu.Lock()
	defer d.mu.Unlock()

	leaked := make([]int, 0)
	for index, dialog := range d.dialogs {
		if dialog.byeCount() == 0 {
			leaked = append(leaked, index)
		}
	}

	return leaked
}

type fakeOutgoingDialog struct {
	mu   sync.Mutex
	byes int
}

func (d *fakeOutgoingDialog) Bye(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.byes++

	return nil
}

func (d *fakeOutgoingDialog) byeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.byes
}

// sinkSettle is how long an exact-count assertion keeps watching after the count
// arrives. The drain runs on its own goroutine, so "the sink has n events" is
// only half the assertion: without the settle a test would not notice an n+1st
// delivery that is still in flight.
const sinkSettle = 50 * time.Millisecond

// waitForCount blocks until count reports exactly the expected number and then
// holds it there for sinkSettle. Every assertion on delivered events goes
// through it, because a publisher returns before its event reaches the sink.
func waitForCount(t *testing.T, want int, count func() int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for count() < want {
		if time.Now().After(deadline) {
			t.Fatalf("sink received %d events, want %d", count(), want)
		}

		time.Sleep(time.Millisecond)
	}

	time.Sleep(sinkSettle)

	if got := count(); got != want {
		t.Fatalf("sink received %d events, want %d", got, want)
	}
}

// syncEventSink records what the drain delivers. It is guarded because the drain
// runs on a goroutine of its own, and read through waitForEvents for the same
// reason.
type syncEventSink struct {
	mu     sync.Mutex
	events []core.Event
}

func (s *syncEventSink) Publish(event core.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.events = append(s.events, event)
}

func (s *syncEventSink) snapshot() []core.Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]core.Event(nil), s.events...)
}

func (s *syncEventSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.events)
}

// waitForEvents returns the delivered events once there are exactly want of
// them, failing the test if they never arrive or if an extra one follows.
func (s *syncEventSink) waitForEvents(t *testing.T, want int) []core.Event {
	t.Helper()

	waitForCount(t, want, s.count)

	return s.snapshot()
}

// panickingSink blows up on its first event, the way a broadcaster does on a
// malformed payload, and reports every later one on delivered.
type panickingSink struct {
	delivered chan core.Event

	mu       sync.Mutex
	panicked bool
}

func newPanickingSink() *panickingSink {
	return &panickingSink{delivered: make(chan core.Event, 8)}
}

func (s *panickingSink) Publish(event core.Event) {
	s.mu.Lock()
	first := !s.panicked
	s.panicked = true
	s.mu.Unlock()

	if first {
		panic("sink exploded on " + fmt.Sprintf("%T", event))
	}

	s.delivered <- event
}

// reentrantSink calls back into the Manager from inside Publish, the way a
// broadcaster does when delivering an event makes it drop a dead client. The
// publish path must tolerate it instead of deadlocking on its own goroutine.
type reentrantSink struct {
	call func()

	mu     sync.Mutex
	called bool
	events []core.Event
}

// Publish re-enters the Manager once, on the first event. The "first" flag is a
// plain guarded bool rather than a sync.Once so that a nested delivery cannot
// block on the helper itself — any hang has to be the Manager's.
func (s *reentrantSink) Publish(event core.Event) {
	s.mu.Lock()
	s.events = append(s.events, event)

	first := !s.called
	s.called = true
	s.mu.Unlock()

	if first && s.call != nil {
		s.call()
	}
}

func (s *reentrantSink) snapshot() []core.Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]core.Event(nil), s.events...)
}

func (s *reentrantSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.events)
}

// gateSink holds its first delivery until the test releases it and records
// whether any other delivery overlapped it. A sink is external code: the
// manager must serialize deliveries without ever running two at once.
type gateSink struct {
	entered chan struct{}
	release chan struct{}
	first   sync.Once

	mu       sync.Mutex
	events   []core.Event
	inFlight int
	overlap  bool
}

func newGateSink() *gateSink {
	return &gateSink{entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *gateSink) Publish(event core.Event) {
	s.mu.Lock()
	s.inFlight++
	s.overlap = s.overlap || s.inFlight > 1
	s.events = append(s.events, event)
	s.mu.Unlock()

	gated := false
	s.first.Do(func() { gated = true })

	if gated {
		close(s.entered)
		<-s.release
	}

	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
}

func (s *gateSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.events)
}

func (s *gateSink) delivered() ([]core.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]core.Event(nil), s.events...), s.overlap
}

func TestManager_HasAnsweredInboundCallTracksTheCallLifecycle(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))

	if manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = true with no call, want false")
	}

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = true while ringing, want false")
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = false after Answer, want true")
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatal(err)
	}

	if manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = true after Hangup, want false")
	}
}

func TestManager_HasAnsweredInboundCallIsFalseAfterRemoteBye(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	manager.RemoteDialogEnded()

	if manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = true after the peer ended the dialog, want false")
	}
}

func TestManager_HasAnsweredInboundCallIsFalseForOutgoingPreview(t *testing.T) {
	t.Parallel()

	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))
	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatal(err)
	}

	if manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = true for an outgoing preview, want false")
	}
}

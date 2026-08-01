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
	events := &fakeEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatalf("OnInvite() error = %v", err)
	}

	if len(dialog.responses) != 1 || dialog.responses[0].status != 180 || dialog.responses[0].reason != "Ringing" {
		t.Fatalf("responses = %#v, want 180 Ringing", dialog.responses)
	}

	if len(events.events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events.events))
	}

	if event, ok := events.events[0].(core.IncomingCallStarted); !ok || event.DialogID != testDialogID || event.EntrypointID != "main" {
		t.Fatalf("event = %#v, want IncomingCallStarted for dialog-1/main", events.events[0])
	}
}

func TestManager_OnInviteRejectsUnattributableCall(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &fakeEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("", ""))

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatalf("OnInvite() error = %v", err)
	}

	if len(dialog.responses) != 1 || dialog.responses[0].status != 486 {
		t.Fatalf("responses = %#v, want 486 Busy Here", dialog.responses)
	}

	if len(events.events) != 0 {
		t.Fatalf("events = %#v, want none", events.events)
	}
}

func TestManager_AnswerMovesIncomingToActiveWithSDP(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &fakeEventSink{}

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

	manager := NewManager("192.0.2.10", &fakeDialer{}, &fakeEventSink{}, testResolver("main", "21"))
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

	manager := NewManager("192.0.2.10", &fakeDialer{}, &fakeEventSink{}, testResolver("main", "21"))
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

	manager := NewManager("192.0.2.10", &fakeDialer{}, &fakeEventSink{}, testResolver("main", "21"))

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
	events := &fakeEventSink{}

	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	manager.EndIncoming(core.CallEndReasonElsewhere)

	if len(events.events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events.events))
	}

	event, ok := events.events[1].(core.IncomingCallEnded)
	if !ok || event.DialogID != testDialogID || event.Reason != core.CallEndReasonElsewhere {
		t.Fatalf("event = %#v, want IncomingCallEnded/elsewhere", events.events[1])
	}

	manager.EndIncoming(core.CallEndReasonCancelled)

	if len(events.events) != 2 {
		t.Fatalf("EndIncoming must be a no-op when nothing is pending, got %d events", len(events.events))
	}

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
	manager := NewManager("192.0.2.10", dialer, &fakeEventSink{}, testResolver("main", "21"))

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

	manager := NewManager("192.0.2.10", dialer, &fakeEventSink{}, testResolver("main", "21"))
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

	manager := NewManager("192.0.2.10", dialer, &fakeEventSink{}, testResolver("main", "21"))
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

	manager := NewManager("192.0.2.10", dialer, &fakeEventSink{}, testResolver("main", "21"))
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
	events := &fakeEventSink{}

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

	if len(events.events) != 1 {
		t.Fatalf("event count = %d, want 1 — the rejected call never started", len(events.events))
	}

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
	events := &fakeEventSink{}

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

	if len(events.events) != 0 {
		t.Fatalf("events = %#v, want none", events.events)
	}

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
	manager := NewManager("192.0.2.10", dialer, &fakeEventSink{}, testResolver("main", "21"))

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
	events := &fakeEventSink{}

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

	if len(events.events) != 0 {
		t.Fatalf("events = %#v, want none — a call that never rang neither starts nor ends", events.events)
	}

	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatalf("StartStream() after the rollback error = %v, want nil", err)
	}
}

func TestManager_AnswerByesDialogWhenItLosesTheIncomingSlot(t *testing.T) {
	t.Parallel()

	dialer := &fakeDialer{}
	manager := NewManager("192.0.2.10", dialer, &fakeEventSink{}, testResolver("main", "21"))

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
	events := &fakeEventSink{}

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

	if len(events.events) != 3 {
		t.Fatalf("event count = %d, want 3", len(events.events))
	}

	if event, ok := events.events[2].(core.CallHungUp); !ok || event.DialogID != testDialogID {
		t.Fatalf("event = %#v, want CallHungUp for dialog-1", events.events[2])
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

	if len(events.events) != 3 {
		t.Fatalf("event count = %d, want 3 — a preview has no dialog the projector knows about", len(events.events))
	}
}

func TestManager_RemoteDialogEndedWithNothingActiveIsANoop(t *testing.T) {
	t.Parallel()

	events := &fakeEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	manager.RemoteDialogEnded()
	manager.RemoteDialogEnded()

	if len(events.events) != 0 {
		t.Fatalf("events = %#v, want none", events.events)
	}

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

	// The sink is still inside IncomingCallStarted. This transition commits
	// next, so its delivery must queue behind it instead of overtaking it.
	ended := make(chan struct{})

	wg.Add(1)

	go func() {
		defer wg.Done()
		defer close(ended)

		manager.EndIncoming(core.CallEndReasonCancelled)
	}()

	select {
	case <-ended:
	case <-time.After(200 * time.Millisecond):
	}

	close(events.release)
	wg.Wait()

	delivered, overlap := events.delivered()
	if overlap {
		t.Fatal("two events were delivered to the sink at once")
	}

	if len(delivered) != 2 {
		t.Fatalf("delivered = %#v, want 2 events", delivered)
	}

	if _, ok := delivered[0].(core.IncomingCallStarted); !ok {
		t.Fatalf("delivered[0] = %#v, want IncomingCallStarted", delivered[0])
	}

	if _, ok := delivered[1].(core.IncomingCallEnded); !ok {
		t.Fatalf("delivered[1] = %#v, want IncomingCallEnded", delivered[1])
	}
}

func TestManager_EndIncomingNormalizesEmptyReason(t *testing.T) {
	t.Parallel()

	events := &fakeEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	if err := manager.OnInvite(context.Background(), &fakeIncomingDialog{id: testDialogID}); err != nil {
		t.Fatal(err)
	}

	manager.EndIncoming("")

	event, ok := events.events[len(events.events)-1].(core.IncomingCallEnded)
	if !ok || event.Reason != core.CallEndReasonCancelled {
		t.Fatalf("event = %#v, want IncomingCallEnded/cancelled", events.events[len(events.events)-1])
	}
}

func TestManager_PreviewTeardownPublishesNothing(t *testing.T) {
	t.Parallel()

	dialer := &fakeDialer{}
	events := &fakeEventSink{}

	manager := NewManager("192.0.2.10", dialer, events, testResolver("main", "21"))
	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatal(err)
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup() error = %v", err)
	}

	if len(events.events) != 0 {
		t.Fatalf("events = %#v, want none — an outbound preview has no dialog the projector knows about", events.events)
	}

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
	calls   int
	offer   string
	dialogs []*fakeOutgoingDialog
}

func (d *fakeDialer) StartStream(_ context.Context, _, offer string) (OutgoingDialog, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.calls++
	d.offer = offer

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

type fakeEventSink struct {
	events []core.Event
}

func (s *fakeEventSink) Publish(event core.Event) {
	s.events = append(s.events, event)
}

// syncEventSink is fakeEventSink for tests that publish from several goroutines.
type syncEventSink struct {
	mu     sync.Mutex
	events []core.Event
}

func (s *syncEventSink) Publish(event core.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.events = append(s.events, event)
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

func (s *gateSink) delivered() ([]core.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]core.Event(nil), s.events...), s.overlap
}

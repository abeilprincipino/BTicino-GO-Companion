package signaling

import (
	"bticino-go-companion/internal/core"
	"context"
	"errors"
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

func TestManager_IncomingCallExpires(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &fakeEventSink{}

	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))
	manager.SetIncomingTimeout(20 * time.Millisecond)

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := manager.Answer(context.Background()); errors.Is(err, ErrNoIncomingDialog) {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("incoming call did not expire")
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// TestManager_IncomingCallExpires only proves that the incoming dialog is gone;
// its first Answer consumes it before the timer can fire. This one leaves the
// call unanswered so the expiry callback really runs.
func TestManager_IncomingCallExpiryPublishesTimeout(t *testing.T) {
	t.Parallel()

	dialog := &syncIncomingDialog{id: testDialogID}
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

// syncIncomingDialog is the fake used by tests where the expiry timer responds
// on its own goroutine.
type syncIncomingDialog struct {
	id core.DialogID

	mu        sync.Mutex
	responses []response
}

func (d *syncIncomingDialog) ID() core.DialogID { return d.id }

func (d *syncIncomingDialog) Respond(_ context.Context, status int, reason, body string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.responses = append(d.responses, response{status: status, reason: reason, body: body})

	return nil
}

func (d *syncIncomingDialog) Bye(context.Context) error { return nil }

func (d *syncIncomingDialog) statuses() []int {
	d.mu.Lock()
	defer d.mu.Unlock()

	statuses := make([]int, 0, len(d.responses))
	for _, resp := range d.responses {
		statuses = append(statuses, resp.status)
	}

	return statuses
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
	calls int
	offer string
}

func (d *fakeDialer) StartStream(_ context.Context, _, offer string) (OutgoingDialog, error) {
	d.calls++
	d.offer = offer

	return &fakeOutgoingDialog{}, nil
}

type fakeOutgoingDialog struct{}

func (*fakeOutgoingDialog) Bye(context.Context) error { return nil }

type fakeEventSink struct {
	events []core.Event
}

func (s *fakeEventSink) Publish(event core.Event) {
	s.events = append(s.events, event)
}

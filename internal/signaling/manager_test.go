package signaling

import (
	"bticino-go-companion/internal/core"
	"context"
	"errors"
	"strings"
	"testing"
)

const testDialogID = "dialog-1"

func TestManager_OnInviteStoresDialogRingsAndPublishes(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &fakeEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events)

	if err := manager.OnInvite(context.Background(), dialog, "main"); err != nil {
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

func TestManager_AnswerMovesIncomingToActiveWithSDP(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &fakeEventSink{}

	manager := NewManager("192.0.2.10", &fakeDialer{}, events)
	if err := manager.OnInvite(context.Background(), dialog, "main"); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatalf("Answer() error = %v", err)
	}

	if len(dialog.responses) != 2 || dialog.responses[1].status != 200 || dialog.responses[1].reason != "OK" {
		t.Fatalf("responses = %#v, want trailing 200 OK", dialog.responses)
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

	manager := NewManager("192.0.2.10", &fakeDialer{}, &fakeEventSink{})
	if err := manager.OnInvite(context.Background(), dialog, "main"); err != nil {
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

	manager := NewManager("192.0.2.10", &fakeDialer{}, &fakeEventSink{})
	if err := manager.OnInvite(context.Background(), dialog, "main"); err != nil {
		t.Fatal(err)
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup() error = %v", err)
	}

	if len(dialog.responses) != 2 || dialog.responses[1].status != 603 || dialog.responses[1].reason != "Decline" {
		t.Fatalf("responses = %#v, want trailing 603 Decline", dialog.responses)
	}
}

func TestManager_StartStreamIsOutgoingOnly(t *testing.T) {
	t.Parallel()

	dialer := &fakeDialer{}
	manager := NewManager("192.0.2.10", dialer, &fakeEventSink{})

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

	manager := NewManager("192.0.2.10", dialer, &fakeEventSink{})
	if err := manager.OnInvite(context.Background(), dialog, "main"); err != nil {
		t.Fatal(err)
	}

	err := manager.StartStream(context.Background(), "21")
	if !errors.Is(err, ErrIncomingDialog) {
		t.Fatalf("StartStream() error = %v, want ErrIncomingDialog", err)
	}

	if len(dialog.responses) != 1 || dialog.responses[0].status != 180 {
		t.Fatalf("responses = %#v, want only 180 Ringing", dialog.responses)
	}

	if dialer.calls != 0 {
		t.Fatalf("dialer calls = %d, want 0", dialer.calls)
	}
}

func TestManager_StartStreamRejectsActiveOutgoingDialog(t *testing.T) {
	t.Parallel()

	dialer := &fakeDialer{}

	manager := NewManager("192.0.2.10", dialer, &fakeEventSink{})
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

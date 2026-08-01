package signaling

import (
	"bticino-go-companion/internal/core"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func TestResolveInviteTargetUsesProfileEndpointAndDomain(t *testing.T) {
	t.Parallel()

	target, err := resolveInviteTarget("c300x@127.0.0.1", "example.local")
	if err != nil {
		t.Fatalf("resolveInviteTarget() error = %v", err)
	}

	if target.URI.User != "c300x" || target.URI.Host != "example.local" {
		t.Fatalf("target URI = %s, want sip:c300x@example.local", target.URI.String())
	}

	if target.destination != "127.0.0.1:5060" {
		t.Fatalf("target destination = %q, want 127.0.0.1:5060", target.destination)
	}
}

func TestNewStreamDialerRequiresTarget(t *testing.T) {
	t.Parallel()

	_, err := NewStreamDialer(StreamDialerConfig{})
	if !errors.Is(err, ErrStreamTargetUnset) {
		t.Fatalf("NewStreamDialer() error = %v, want %v", err, ErrStreamTargetUnset)
	}
}

func TestStreamDialerSetRemoteDialogEndedReplacesActiveStreamCallback(t *testing.T) {
	dialer := &streamDialer{}
	first, second := 0, 0

	dialer.SetRemoteDialogEnded(func() { first++ })
	dialer.SetRemoteDialogEnded(func() { second++ })

	dialer.callbackMu.RLock()
	callback := dialer.remoteDialogEnded
	dialer.callbackMu.RUnlock()
	callback()

	if first != 0 || second != 1 {
		t.Fatalf("callback counts = first:%d second:%d, want first:0 second:1", first, second)
	}
}

func TestRegistrationLoopRefreshesAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := make(chan struct{}, 2)
	done := make(chan struct{})

	go func() {
		registrationLoop(ctx, time.Millisecond, time.Millisecond, time.Second, func(context.Context) error {
			select {
			case calls <- struct{}{}:
			default:
			}

			return nil
		})
		close(done)
	}()

	for range 2 {
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatal("registration was not refreshed")
		}
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("registration loop did not stop after cancellation")
	}
}

func TestWaitForDialogEnd(t *testing.T) {
	t.Run("dialog ended", func(t *testing.T) {
		done := make(chan struct{})
		close(done)

		if err := waitForDialogEnd(context.Background(), done); err != nil {
			t.Fatalf("waitForDialogEnd() error = %v", err)
		}
	})

	t.Run("context canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := waitForDialogEnd(ctx, make(chan struct{})); !errors.Is(err, context.Canceled) {
			t.Fatalf("waitForDialogEnd() error = %v, want context.Canceled", err)
		}
	})
}

func TestCancelReasonDetectsAnsweredElsewhere(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		want   core.CallEndReason
	}{
		{name: "answered elsewhere", header: `SIP;cause=200;text="Call completed elsewhere"`, want: core.CallEndReasonElsewhere},
		{name: "caller gave up", header: `SIP;cause=487;text="Request Terminated"`, want: core.CallEndReasonCancelled},
		{name: "no header", header: "", want: core.CallEndReasonCancelled},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			req := sip.NewRequest(sip.CANCEL, sip.Uri{Scheme: "sip", User: "companion", Host: "127.0.0.1"})
			if test.header != "" {
				req.AppendHeader(sip.NewHeader("Reason", test.header))
			}

			if got := cancelReason(req); got != test.want {
				t.Fatalf("cancelReason() = %q, want %q", got, test.want)
			}
		})
	}
}

// fakeServerSession records what the adapter writes on the SIP session and how
// many of those writes overlap, because sipgo does not serialize them itself.
type fakeServerSession struct {
	mu         sync.Mutex
	writes     []string
	active     int
	maxActive  int
	closes     int
	byes       int
	state      sip.DialogState
	writeDelay time.Duration
}

func (s *fakeServerSession) record(entry string) error {
	s.mu.Lock()
	s.writes = append(s.writes, entry)
	s.active++

	if s.active > s.maxActive {
		s.maxActive = s.active
	}

	delay := s.writeDelay
	s.mu.Unlock()

	time.Sleep(delay)

	s.mu.Lock()
	s.active--
	s.mu.Unlock()

	return nil
}

func (s *fakeServerSession) Respond(_ int, reason string, _ []byte, _ ...sip.Header) error {
	return s.record(reason)
}

func (s *fakeServerSession) RespondSDP(sdp []byte) error {
	return s.record(string(sdp))
}

func (s *fakeServerSession) Bye(context.Context) error {
	s.mu.Lock()
	s.byes++
	s.mu.Unlock()

	return nil
}

func (s *fakeServerSession) Close() error {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()

	return nil
}

func (s *fakeServerSession) LoadState() sip.DialogState {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.state
}

func (s *fakeServerSession) snapshot() ([]string, int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.writes...), s.maxActive, s.closes, s.byes
}

type fakeInboundHandler struct {
	mu      sync.Mutex
	reasons []core.CallEndReason
}

func (h *fakeInboundHandler) OnInvite(context.Context, IncomingDialog) error { return nil }

func (h *fakeInboundHandler) EndIncoming(reason core.CallEndReason) {
	h.mu.Lock()
	h.reasons = append(h.reasons, reason)
	h.mu.Unlock()
}

func (h *fakeInboundHandler) collected() []core.CallEndReason {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]core.CallEndReason(nil), h.reasons...)
}

func testDialer(handler InboundHandler) *streamDialer {
	dialer := &streamDialer{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if handler != nil {
		dialer.SetInboundHandler(handler)
	}

	return dialer
}

func TestIncomingDialogRespondNeverOverlapsSessionWrites(t *testing.T) {
	t.Parallel()

	session := &fakeServerSession{writeDelay: 20 * time.Millisecond}
	dialog := &incomingDialog{session: session, id: "call-1"}

	var group sync.WaitGroup

	group.Add(2)

	go func() {
		defer group.Done()

		_ = dialog.Respond(context.Background(), 180, "Ringing", "")
	}()

	go func() {
		defer group.Done()

		_ = dialog.Respond(context.Background(), 200, "OK", "v=0")
	}()

	group.Wait()

	if _, maxActive, _, _ := session.snapshot(); maxActive != 1 {
		t.Fatalf("overlapping session writes = %d, want 1", maxActive)
	}
}

func TestIncomingDialogRespondSendsOneFinalResponse(t *testing.T) {
	t.Parallel()

	session := &fakeServerSession{}
	dialog := &incomingDialog{session: session, id: "call-1"}

	const responders = 8

	var (
		group     sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		refused   int
	)

	group.Add(responders)

	for range responders {
		go func() {
			defer group.Done()

			err := dialog.Respond(context.Background(), 480, "Temporarily Unavailable", "")

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrDialogConcluded):
				refused++
			default:
				t.Errorf("Respond() error = %v, want nil or ErrDialogConcluded", err)
			}
		}()
	}

	group.Wait()

	writes, _, closes, _ := session.snapshot()
	if len(writes) != 1 || succeeded != 1 || refused != responders-1 {
		t.Fatalf("writes = %v, succeeded = %d, refused = %d, want 1 write, 1 success, %d refusals", writes, succeeded, refused, responders-1)
	}

	if closes != 1 {
		t.Fatalf("session closes = %d, want 1", closes)
	}
}

func TestIncomingDialogRespondAnswersWithSDP(t *testing.T) {
	t.Parallel()

	session := &fakeServerSession{}
	dialog := &incomingDialog{session: session, id: "call-1"}

	if err := dialog.Respond(context.Background(), 200, "OK", "v=0\r\n"); err != nil {
		t.Fatalf("Respond() error = %v", err)
	}

	writes, _, closes, _ := session.snapshot()
	if len(writes) != 1 || writes[0] != "v=0\r\n" {
		t.Fatalf("session writes = %v, want the SDP answer", writes)
	}

	if closes != 0 {
		t.Fatalf("session closes = %d, want 0 for an answered dialog", closes)
	}
}

func TestIncomingDialogEndPendingClaimsRingingDialogOnce(t *testing.T) {
	t.Parallel()

	t.Run("never rang", func(t *testing.T) {
		t.Parallel()

		dialog := &incomingDialog{session: &fakeServerSession{}, id: "call-1"}
		if dialog.endPending() {
			t.Fatal("endPending() = true for a dialog that never rang")
		}
	})

	t.Run("ringing", func(t *testing.T) {
		t.Parallel()

		dialog := &incomingDialog{session: &fakeServerSession{}, id: "call-1"}
		if err := dialog.Respond(context.Background(), 180, "Ringing", ""); err != nil {
			t.Fatalf("Respond() error = %v", err)
		}

		if !dialog.endPending() {
			t.Fatal("endPending() = false for a ringing dialog")
		}

		if dialog.endPending() {
			t.Fatal("endPending() = true twice for the same dialog")
		}

		if err := dialog.Respond(context.Background(), 200, "OK", "v=0"); !errors.Is(err, ErrDialogConcluded) {
			t.Fatalf("Respond() after endPending error = %v, want ErrDialogConcluded", err)
		}
	})

	t.Run("answered", func(t *testing.T) {
		t.Parallel()

		dialog := &incomingDialog{session: &fakeServerSession{}, id: "call-1"}
		if err := dialog.Respond(context.Background(), 180, "Ringing", ""); err != nil {
			t.Fatalf("Respond() error = %v", err)
		}

		if err := dialog.Respond(context.Background(), 200, "OK", "v=0"); err != nil {
			t.Fatalf("Respond() error = %v", err)
		}

		if dialog.endPending() {
			t.Fatal("endPending() = true for an answered dialog")
		}
	})
}

func TestIncomingDialogByeSkipsDialogThatWasNeverAnswered(t *testing.T) {
	t.Parallel()

	session := &fakeServerSession{}
	dialog := &incomingDialog{session: session, id: "call-1"}

	if err := dialog.Bye(context.Background()); err != nil {
		t.Fatalf("Bye() error = %v", err)
	}

	_, _, closes, byes := session.snapshot()
	if byes != 0 || closes != 1 {
		t.Fatalf("byes = %d, closes = %d, want 0 byes and 1 close", byes, closes)
	}

	answered := &fakeServerSession{state: sip.DialogStateConfirmed}
	if err := (&incomingDialog{session: answered, id: "call-2"}).Bye(context.Background()); err != nil {
		t.Fatalf("Bye() error = %v", err)
	}

	if _, _, closes, byes := answered.snapshot(); byes != 1 || closes != 1 {
		t.Fatalf("byes = %d, closes = %d, want 1 bye and 1 close", byes, closes)
	}
}

func TestStreamDialerEndPendingIncomingOnlyReportsTheRingingCall(t *testing.T) {
	t.Parallel()

	handler := &fakeInboundHandler{}
	dialer := testDialer(handler)

	ringing := &incomingDialog{session: &fakeServerSession{}, id: "call-1"}
	if err := ringing.Respond(context.Background(), 180, "Ringing", ""); err != nil {
		t.Fatalf("Respond() error = %v", err)
	}

	answered := &incomingDialog{session: &fakeServerSession{}, id: "call-2"}
	if err := answered.Respond(context.Background(), 180, "Ringing", ""); err != nil {
		t.Fatalf("Respond() error = %v", err)
	}

	if err := answered.Respond(context.Background(), 200, "OK", "v=0"); err != nil {
		t.Fatalf("Respond() error = %v", err)
	}

	// A dialog rejected with 486 Busy Here never rang, so a CANCEL for it must
	// not clear the call that is actually pending.
	busy := &incomingDialog{session: &fakeServerSession{}, id: "call-3"}
	if err := busy.Respond(context.Background(), 486, "Busy Here", ""); err != nil {
		t.Fatalf("Respond() error = %v", err)
	}

	if dialer.endPendingIncoming(answered, core.CallEndReasonCancelled, "cancel") {
		t.Fatal("endPendingIncoming() reported an answered dialog")
	}

	if dialer.endPendingIncoming(busy, core.CallEndReasonCancelled, "cancel") {
		t.Fatal("endPendingIncoming() reported a rejected dialog")
	}

	if !dialer.endPendingIncoming(ringing, core.CallEndReasonElsewhere, "cancel") {
		t.Fatal("endPendingIncoming() did not report the ringing dialog")
	}

	if dialer.endPendingIncoming(ringing, core.CallEndReasonCancelled, "bye") {
		t.Fatal("endPendingIncoming() reported the same dialog twice")
	}

	if reasons := handler.collected(); len(reasons) != 1 || reasons[0] != core.CallEndReasonElsewhere {
		t.Fatalf("EndIncoming reasons = %v, want [%q]", reasons, core.CallEndReasonElsewhere)
	}
}

func TestStreamDialerEndPendingIncomingWithoutHandler(t *testing.T) {
	t.Parallel()

	dialer := testDialer(nil)
	dialog := &incomingDialog{session: &fakeServerSession{}, id: "call-1"}

	if err := dialog.Respond(context.Background(), 180, "Ringing", ""); err != nil {
		t.Fatalf("Respond() error = %v", err)
	}

	if dialer.endPendingIncoming(dialog, core.CallEndReasonCancelled, "cancel") {
		t.Fatal("endPendingIncoming() reported a call with no inbound handler")
	}
}

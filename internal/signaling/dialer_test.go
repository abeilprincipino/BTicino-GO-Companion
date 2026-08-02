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

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/emiago/sipgo/siptest"
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
//
// entered and release make a write observable and suspendable: a test that has
// to prove one write waits for another cannot do it with a sleep, because the
// two orderings it must tell apart differ only in timing.
type fakeServerSession struct {
	mu        sync.Mutex
	writes    []string
	active    int
	maxActive int
	closes    int
	byes      int
	state     sip.DialogState

	entered chan string
	release chan struct{}
}

func (s *fakeServerSession) record(entry string) error {
	s.mu.Lock()
	s.writes = append(s.writes, entry)
	s.active++

	if s.active > s.maxActive {
		s.maxActive = s.active
	}

	entered, release := s.entered, s.release
	s.mu.Unlock()

	if entered != nil {
		entered <- entry
	}

	if release != nil {
		<-release
	}

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

func (h *fakeInboundHandler) RemoteDialogEnded() {}

func (h *fakeInboundHandler) collected() []core.CallEndReason {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]core.CallEndReason(nil), h.reasons...)
}

// discardLogger keeps a dialog's own log lines — an answer left unacknowledged
// is warned about, by a goroutine that outlives the call that abandoned it — off
// the test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testDialer(handler InboundHandler) *streamDialer {
	dialer := &streamDialer{logger: discardLogger()}
	if handler != nil {
		dialer.SetInboundHandler(handler)
	}

	return dialer
}

func TestIncomingDialogRespondNeverOverlapsSessionWrites(t *testing.T) {
	t.Parallel()

	// The 180 is started first and held inside the session write, so the answer
	// is guaranteed to arrive while a write is genuinely in flight. Letting the
	// two race instead would let the 200 conclude the dialog first, refuse the
	// 180 and leave a single write behind, which satisfies the overlap
	// assertion without ever having exercised the serialization.
	session := &fakeServerSession{entered: make(chan string, 2), release: make(chan struct{})}
	dialog := &incomingDialog{session: session, id: "call-1"}

	ringing := make(chan error, 1)

	go func() { ringing <- dialog.Respond(context.Background(), 180, "Ringing", "") }()

	if entry := <-session.entered; entry != "Ringing" {
		t.Fatalf("first session write = %q, want the 180 Ringing", entry)
	}

	answer := make(chan error, 1)

	go func() { answer <- dialog.Respond(context.Background(), 200, "OK", "v=0") }()

	select {
	case entry := <-session.entered:
		t.Fatalf("session write %q started while the 180 Ringing was still on the wire", entry)
	case err := <-answer:
		t.Fatalf("Respond(200) returned (error = %v) while the 180 Ringing was still on the wire", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(session.release)

	if err := <-ringing; err != nil {
		t.Fatalf("Respond(180) error = %v", err)
	}

	if err := <-answer; err != nil {
		t.Fatalf("Respond(200) error = %v", err)
	}

	writes, maxActive, _, _ := session.snapshot()
	if maxActive != 1 {
		t.Fatalf("overlapping session writes = %d, want 1", maxActive)
	}

	if len(writes) != 2 || writes[0] != "Ringing" || writes[1] != "v=0" {
		t.Fatalf("session writes = %v, want the 180 Ringing followed by the answer", writes)
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

// TestIncomingDialogRespondRoutesOnTheStatusNotTheBody pins the rule the rest of
// the adapter depends on: what a response *is* follows from its status code.
// Discriminating on the body instead sends a bodyless 200 OK down the rejection
// path — closing and dropping from sipgo's server cache the very dialog that
// response just established, after which no BYE for it can be matched — and a
// provisional carrying early SDP down the answer path.
func TestIncomingDialogRespondRoutesOnTheStatusNotTheBody(t *testing.T) {
	t.Parallel()

	t.Run("bodyless success answers", func(t *testing.T) {
		t.Parallel()

		session := &fakeServerSession{}
		dialog := &incomingDialog{session: session, id: "call-1"}

		if err := dialog.Respond(context.Background(), 200, "OK", ""); err != nil {
			t.Fatalf("Respond() error = %v", err)
		}

		writes, _, closes, _ := session.snapshot()
		if len(writes) != 1 || writes[0] != "OK" {
			t.Fatalf("session writes = %v, want a single 200 OK", writes)
		}

		if closes != 0 {
			t.Fatalf("session closes = %d, want 0: a 200 OK establishes the dialog and must not drop it from the server cache", closes)
		}
	})

	t.Run("provisional with a body stays provisional", func(t *testing.T) {
		t.Parallel()

		session := &fakeServerSession{}
		dialog := &incomingDialog{session: session, id: "call-1"}

		if err := dialog.Respond(context.Background(), 183, "Session Progress", "v=0"); err != nil {
			t.Fatalf("Respond() error = %v", err)
		}

		writes, _, _, _ := session.snapshot()
		if len(writes) != 1 || writes[0] != "Session Progress" {
			t.Fatalf("session writes = %v, want the 183 sent as a provisional, not as an answer", writes)
		}

		if !dialog.endPending() {
			t.Fatal("endPending() = false: a 183 is provisional and leaves the call pending")
		}
	})
}

// TestIncomingDialogAnswerStopsWaitingOnceTheAnswerIsOnTheWire pins the bound on
// an answer. sipgo keeps retransmitting a 200 OK until the ACK arrives or its
// INVITE transaction is retired 32s later, with no context anywhere, and the
// error it finally returns is an error about the ACK — not about the answer,
// which is long gone. Reporting that to the caller fails an answered call: the
// card never starts media for it, and never shows the button that hangs it up.
func TestIncomingDialogAnswerStopsWaitingOnceTheAnswerIsOnTheWire(t *testing.T) {
	t.Parallel()

	// The write is held for as long as sipgo's retransmit loop would hold it, on
	// an Established dialog: the answer has been handed to the transaction and
	// only the ACK is outstanding.
	session := &fakeServerSession{state: sip.DialogStateEstablished, release: make(chan struct{})}
	defer close(session.release)

	dialog := &incomingDialog{session: session, id: "call-1", logger: discardLogger()}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	answered := make(chan error, 1)

	go func() { answered <- dialog.Respond(ctx, 200, "OK", "v=0") }()

	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("Respond(200) error = %v, want nil: the answer is on the wire, and failing it strands a call the intercom has connected", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Respond(200) never returned: the caller's context does not bound the wait for the ACK, so an HTTP request sits here for sipgo's full 32s")
	}
}

// TestIncomingDialogAnswerWaitsWithoutEvidenceTheAnswerWasSent pins the other
// half of the same rule. Expiry is reported as success only because sipgo marks
// the dialog Established before it responds on the transaction; a dialog it has
// not marked says nothing about the answer having been sent, and there is then
// nothing to report but the wait itself.
func TestIncomingDialogAnswerWaitsWithoutEvidenceTheAnswerWasSent(t *testing.T) {
	t.Parallel()

	session := &fakeServerSession{release: make(chan struct{})}
	dialog := &incomingDialog{session: session, id: "call-1", logger: discardLogger()}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	answered := make(chan error, 1)

	go func() { answered <- dialog.Respond(ctx, 200, "OK", "v=0") }()

	select {
	case err := <-answered:
		t.Fatalf("Respond(200) returned (error = %v) for an answer nothing says was ever sent", err)
	case <-time.After(250 * time.Millisecond):
	}

	close(session.release)

	if err := <-answered; err != nil {
		t.Fatalf("Respond(200) error = %v", err)
	}
}

// TestIncomingDialogRejectionStopsWaitingForItsAck pins the bound on the other
// blocking final response: Hangup answers a ringing call with a 603, and sipgo
// then waits for an ACK it treats as optional — that wait ends in success
// whether the ACK arrives or the transaction times out 32s later, so a caller
// that stops waiting for it gives up on nothing it would have been told about.
func TestIncomingDialogRejectionStopsWaitingForItsAck(t *testing.T) {
	t.Parallel()

	session := &fakeServerSession{release: make(chan struct{})}
	defer close(session.release)

	dialog := &incomingDialog{session: session, id: "call-1", logger: discardLogger()}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	declined := make(chan error, 1)

	go func() { declined <- dialog.Respond(ctx, 603, "Decline", "") }()

	select {
	case err := <-declined:
		if err != nil {
			t.Fatalf("Respond(603) error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Respond(603) never returned: the caller's context does not bound the wait for the ACK, so hanging up a ringing call sits here for sipgo's full 32s")
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

// The tests below drive the real sipgo INVITE server transaction over siptest's
// recording connection, so the two library behaviours the inbound half rests on
// are defended by something other than a comment: that a matching CANCEL is
// reported through the transaction's own cancel hook rather than through the
// server's CANCEL handler, and that onInvite must not return while the call is
// only ringing. Neither needs a socket.

// ringingInboundHandler answers an INVITE the way the manager does — a 180 and
// nothing more — and reports back on channels, so a test can drive the SIP state
// machine in a defined order instead of sleeping on it.
type ringingInboundHandler struct {
	mu      sync.Mutex
	dialogs []IncomingDialog
	reasons []core.CallEndReason

	rang        chan IncomingDialog
	ended       chan core.CallEndReason
	remoteEnded chan struct{}
}

func newRingingInboundHandler() *ringingInboundHandler {
	return &ringingInboundHandler{
		rang:        make(chan IncomingDialog, 4),
		ended:       make(chan core.CallEndReason, 4),
		remoteEnded: make(chan struct{}, 4),
	}
}

func (h *ringingInboundHandler) OnInvite(ctx context.Context, dialog IncomingDialog) error {
	h.mu.Lock()
	h.dialogs = append(h.dialogs, dialog)
	h.mu.Unlock()

	if err := dialog.Respond(ctx, 180, "Ringing", ""); err != nil {
		return err
	}

	h.rang <- dialog

	return nil
}

func (h *ringingInboundHandler) EndIncoming(reason core.CallEndReason) {
	h.mu.Lock()
	h.reasons = append(h.reasons, reason)
	h.mu.Unlock()

	h.ended <- reason
}

func (h *ringingInboundHandler) RemoteDialogEnded() {
	h.remoteEnded <- struct{}{}
}

func (h *ringingInboundHandler) invited() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.dialogs)
}

// testInboundDialer builds an inbound-capable dialer over a real user agent.
// Neither sipgo.NewUA nor NewClient or NewServer opens a socket — only
// ListenAndServe does, and nothing here calls it.
func testInboundDialer(t *testing.T, handler InboundHandler) *streamDialer {
	t.Helper()

	agent, err := sipgo.NewUA(sipgo.WithUserAgent("companion"), sipgo.WithUserAgentHostname("127.0.0.1"))
	if err != nil {
		t.Fatalf("sipgo.NewUA() error = %v", err)
	}

	client, err := sipgo.NewClient(agent)
	if err != nil {
		t.Fatalf("sipgo.NewClient() error = %v", err)
	}

	server, err := sipgo.NewServer(agent)
	if err != nil {
		t.Fatalf("sipgo.NewServer() error = %v", err)
	}

	contact := sip.ContactHeader{Address: sip.Uri{User: "companion", Host: "127.0.0.1", Port: 5070}}
	dialer := &streamDialer{
		ua:             agent,
		server:         server,
		client:         client,
		out:            sipgo.NewDialogClientCache(client, contact),
		in:             sipgo.NewDialogServerCache(client, contact),
		contact:        contact,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		listenerCancel: func() {},
		registerCancel: func() {},
	}

	if handler != nil {
		dialer.SetInboundHandler(handler)
	}

	return dialer
}

// testInviteRequest builds the INVITE a Flexisip fork of a doorbell call looks
// like. The transport is TCP because it is reliable, which keeps the transaction
// from retransmitting its own responses into the recorder.
func testInviteRequest() *sip.Request {
	req := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: "companion", Host: "127.0.0.1", Port: 5070})
	req.SetTransport("TCP")

	via := sip.NewParams()
	via.Add("branch", sip.GenerateBranch())
	req.AppendHeader(&sip.ViaHeader{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       "TCP",
		Host:            "127.0.0.1",
		Port:            5060,
		Params:          via,
	})

	from := sip.NewParams()
	from.Add("tag", sip.GenerateTagN(12))
	req.AppendHeader(&sip.FromHeader{
		DisplayName: "Doorbell",
		Address:     sip.Uri{Scheme: "sip", User: "doorbell", Host: "127.0.0.1", Port: 5060},
		Params:      from,
	})
	req.AppendHeader(&sip.ToHeader{
		Address: sip.Uri{Scheme: "sip", User: "companion", Host: "127.0.0.1", Port: 5070},
		Params:  sip.NewParams(),
	})

	callID := sip.CallIDHeader("call-" + sip.GenerateTagN(12))
	req.AppendHeader(&callID)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 10, MethodName: sip.INVITE})
	req.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{Scheme: "sip", User: "doorbell", Host: "127.0.0.1", Port: 5060},
		Params:  sip.NewParams(),
	})
	req.SetSource("127.0.0.1:5060")
	req.SetBody(nil)

	return req
}

// testCancelRequest builds the CANCEL Flexisip sends to the losing handsets,
// whose Reason header is what tells "the visitor gave up" from "somebody else
// picked up".
func testCancelRequest(invite *sip.Request, reason string) *sip.Request {
	req := sip.NewRequest(sip.CANCEL, invite.Recipient)
	req.SetTransport(invite.Transport())
	req.AppendHeader(sip.HeaderClone(invite.Via()))
	req.AppendHeader(sip.HeaderClone(invite.From()))
	req.AppendHeader(sip.HeaderClone(invite.To()))
	req.AppendHeader(sip.HeaderClone(invite.CallID()))
	req.AppendHeader(&sip.CSeqHeader{SeqNo: invite.CSeq().SeqNo, MethodName: sip.CANCEL})

	if reason != "" {
		req.AppendHeader(sip.NewHeader("Reason", reason))
	}

	req.SetSource(invite.Source())

	return req
}

// testByeRequest builds a BYE inside the dialog the session belongs to. It is
// built from the session's own invite request, which is the clone carrying the
// To tag sipgo generated, so the two dialog IDs match.
func testByeRequest(session *sipgo.DialogServerSession) *sip.Request {
	invite := session.InviteRequest

	req := sip.NewRequest(sip.BYE, invite.Recipient)
	req.SetTransport(invite.Transport())

	via := sip.NewParams()
	via.Add("branch", sip.GenerateBranch())
	req.AppendHeader(&sip.ViaHeader{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       "TCP",
		Host:            "127.0.0.1",
		Port:            5060,
		Params:          via,
	})
	req.AppendHeader(sip.HeaderClone(invite.From()))
	req.AppendHeader(sip.HeaderClone(invite.To()))
	req.AppendHeader(sip.HeaderClone(invite.CallID()))
	req.AppendHeader(&sip.CSeqHeader{SeqNo: invite.CSeq().SeqNo + 1, MethodName: sip.BYE})
	req.SetSource(invite.Source())
	req.SetBody(nil)

	return req
}

// testAckRequest builds the ACK that confirms an answered dialog. Its CSeq must
// be the INVITE's, which is what sipgo matches it on.
func testAckRequest(session *sipgo.DialogServerSession) *sip.Request {
	invite := session.InviteRequest

	req := sip.NewRequest(sip.ACK, invite.Recipient)
	req.SetTransport(invite.Transport())

	via := sip.NewParams()
	via.Add("branch", sip.GenerateBranch())
	req.AppendHeader(&sip.ViaHeader{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       "TCP",
		Host:            "127.0.0.1",
		Port:            5060,
		Params:          via,
	})
	req.AppendHeader(sip.HeaderClone(invite.From()))
	req.AppendHeader(sip.HeaderClone(invite.To()))
	req.AppendHeader(sip.HeaderClone(invite.CallID()))
	req.AppendHeader(&sip.CSeqHeader{SeqNo: invite.CSeq().SeqNo, MethodName: sip.ACK})
	req.SetSource(invite.Source())
	req.SetBody(nil)

	return req
}

func serverSessionOf(t *testing.T, dialog IncomingDialog) *sipgo.DialogServerSession {
	t.Helper()

	incoming, ok := dialog.(*incomingDialog)
	if !ok {
		t.Fatalf("inbound dialog type = %T, want *incomingDialog", dialog)
	}

	session, ok := incoming.session.(*sipgo.DialogServerSession)
	if !ok {
		t.Fatalf("inbound session type = %T, want *sipgo.DialogServerSession", incoming.session)
	}

	return session
}

// inboundSessionCached reports whether sipgo's server cache still holds the
// session. Only DialogServerSession.Close removes it from there.
func inboundSessionCached(t *testing.T, dialer *streamDialer, session *sipgo.DialogServerSession) bool {
	t.Helper()

	_, err := dialer.in.MatchDialogRequest(session.InviteRequest)

	switch {
	case err == nil:
		return true
	case errors.Is(err, sipgo.ErrDialogDoesNotExists):
		return false
	default:
		t.Fatalf("MatchDialogRequest() error = %v", err)

		return false
	}
}

func waitRinging(t *testing.T, handler *ringingInboundHandler) IncomingDialog {
	t.Helper()

	select {
	case dialog := <-handler.rang:
		return dialog
	case <-time.After(2 * time.Second):
		t.Fatal("the inbound handler never rang the call")

		return nil
	}
}

func waitDialogState(t *testing.T, session *sipgo.DialogServerSession, want sip.DialogState) {
	t.Helper()

	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if session.LoadState() >= want {
			return
		}
	}

	t.Fatalf("dialog state = %v, want at least %v", session.LoadState(), want)
}

// inboundDialogCount reports how many entries are currently tracked in the
// dialer's inbound dialog map. Its keys are per-call dialog IDs, so a stale
// entry can never be matched by a later call - the map should be empty once
// every dialog it ever held has ended.
func inboundDialogCount(dialer *streamDialer) int {
	count := 0

	dialer.inboundDialogs.Range(func(_, _ any) bool {
		count++

		return true
	})

	return count
}

func waitClosed(t *testing.T, done <-chan struct{}, message string) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

func responseStatuses(responses []*sip.Response) []int {
	statuses := make([]int, len(responses))
	for index, response := range responses {
		statuses[index] = response.StatusCode
	}

	return statuses
}

func TestStreamDialerOnInviteReportsACancelAndLeavesNoSessionBehind(t *testing.T) {
	handler := newRingingInboundHandler()
	dialer := testInboundDialer(t, handler)
	invite := testInviteRequest()
	recorder := siptest.NewServerTxRecorder(invite)

	returned := make(chan struct{})

	go func() {
		defer close(returned)

		dialer.onInvite(invite, recorder)
	}()

	session := serverSessionOf(t, waitRinging(t, handler))

	if err := recorder.Receive(testCancelRequest(invite, `SIP;cause=200;text="Call completed elsewhere"`)); err != nil {
		t.Fatalf("Receive(CANCEL) error = %v", err)
	}

	select {
	case reason := <-handler.ended:
		if reason != core.CallEndReasonElsewhere {
			t.Fatalf("EndIncoming reason = %q, want %q", reason, core.CallEndReasonElsewhere)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the CANCEL never reached EndIncoming, so the pending call would survive until the manager's expiry")
	}

	waitClosed(t, returned, "onInvite() did not return after the CANCEL")

	// siptest.NewServerTxRecorder arms sipgo's Timer_1xx at 200ms, which injects
	// an extra 100 Trying ahead of the 180 if the provisional is delayed past
	// that on a loaded machine. Checking only the last two statuses tolerates
	// that without weakening what the assertion actually pins: the 180 must
	// still be followed by the 487.
	if statuses := responseStatuses(recorder.Result()); len(statuses) < 2 || statuses[len(statuses)-2] != 180 || statuses[len(statuses)-1] != 487 {
		t.Fatalf("recorded responses = %v, want the last two to be [180 487]", statuses)
	}

	if inboundSessionCached(t, dialer, session) {
		t.Fatal("the cancelled dialog is still in sipgo's server cache: a CANCEL passes through neither a rejection nor ReadBye, so nothing else ever closes the session")
	}
}

// TestStreamDialerOnInviteLeavesAnAnsweredDialogAlive pins the other half of the
// same rule. onInvite does not live for the whole life of an answered dialog:
// after the 2xx the INVITE transaction enters RFC 6026's Accepted state and arms
// Timer_L = 64*T1 = 32s, and when that fires tx.Done() closes on a call that is
// still up. Terminating the transaction by hand is that moment, 32 seconds early.
func TestStreamDialerOnInviteLeavesAnAnsweredDialogAlive(t *testing.T) {
	handler := newRingingInboundHandler()
	dialer := testInboundDialer(t, handler)
	invite := testInviteRequest()
	recorder := siptest.NewServerTxRecorder(invite)

	returned := make(chan struct{})

	go func() {
		defer close(returned)

		dialer.onInvite(invite, recorder)
	}()

	dialog := waitRinging(t, handler)
	session := serverSessionOf(t, dialog)

	answered := make(chan error, 1)

	go func() { answered <- dialog.Respond(context.Background(), 200, "OK", "v=0\r\n") }()

	// The answer blocks until the far end acknowledges it, so the ACK has to be
	// fed in from here. Waiting on the dialog state rather than on the recorded
	// responses keeps this off the recorder's unsynchronized message slice.
	waitDialogState(t, session, sip.DialogStateEstablished)

	ack := testAckRequest(session)
	dialer.onAck(ack, siptest.NewServerTxRecorder(ack))

	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("Respond(200) error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the answer never completed after the ACK")
	}

	recorder.Terminate()

	waitClosed(t, returned, "onInvite() did not return once its INVITE transaction had retired")

	if !inboundSessionCached(t, dialer, session) {
		t.Fatal("the answered dialog was dropped from sipgo's server cache when its INVITE transaction retired, so no BYE for it could ever be matched again")
	}

	if dialer.loadInboundDialog(session.ID) == nil {
		t.Fatal("the answered dialog was dropped from the inbound map when its INVITE transaction retired, 32 seconds into a live call")
	}
}

// TestStreamDialerOnInviteDeletesDialogWhenSessionEndsAfterHandlerReturned
// pins the OnState hook itself, not merely the teardown defer. An answered
// call's onInvite goroutine returns 32 seconds early, while the dialog itself
// stays alive - so the defer's close branch never runs for it, and it takes
// the early-return in its switch instead. The only code left that can ever
// remove such a dialog from the map once it later ends is the OnState hook
// registered while the dialog was still live.
func TestStreamDialerOnInviteDeletesDialogWhenSessionEndsAfterHandlerReturned(t *testing.T) {
	handler := newRingingInboundHandler()
	dialer := testInboundDialer(t, handler)
	invite := testInviteRequest()
	recorder := siptest.NewServerTxRecorder(invite)

	returned := make(chan struct{})

	go func() {
		defer close(returned)

		dialer.onInvite(invite, recorder)
	}()

	dialog := waitRinging(t, handler)
	session := serverSessionOf(t, dialog)

	answered := make(chan error, 1)

	go func() { answered <- dialog.Respond(context.Background(), 200, "OK", "v=0\r\n") }()

	waitDialogState(t, session, sip.DialogStateEstablished)

	ack := testAckRequest(session)
	dialer.onAck(ack, siptest.NewServerTxRecorder(ack))

	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("Respond(200) error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the answer never completed after the ACK")
	}

	// Retiring the transaction here, exactly as
	// TestStreamDialerOnInviteLeavesAnAnsweredDialogAlive does, sends onInvite's
	// own goroutine through its defer while the dialog is still Established:
	// that switch takes the early return and never touches the map.
	recorder.Terminate()

	waitClosed(t, returned, "onInvite() did not return once its INVITE transaction had retired")

	if dialer.loadInboundDialog(session.ID) == nil {
		t.Fatal("the answered dialog was dropped from the inbound map when its INVITE transaction retired, before it had actually ended")
	}

	// End the call now, entirely through a separate goroutine (onBye), long
	// after onInvite's own goroutine has already returned. Nothing but the
	// OnState hook registered inside onInvite can delete the map entry here.
	bye := testByeRequest(session)
	dialer.onBye(bye, siptest.NewServerTxRecorder(bye))

	deadline := time.Now().Add(2 * time.Second)
	for dialer.loadInboundDialog(session.ID) != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if dialer.loadInboundDialog(session.ID) != nil {
		t.Fatal("the dialog was not removed from the inbound map once the session reached Ended: the OnState hook never fired")
	}
}

func TestStreamDialerOnInviteStaysWithARingingCall(t *testing.T) {
	handler := newRingingInboundHandler()
	dialer := testInboundDialer(t, handler)
	invite := testInviteRequest()
	recorder := siptest.NewServerTxRecorder(invite)

	returned := make(chan struct{})

	go func() {
		defer close(returned)

		dialer.onInvite(invite, recorder)
	}()

	waitRinging(t, handler)

	select {
	case <-returned:
		t.Fatal("onInvite() returned while the call was only ringing: sipgo terminates the INVITE transaction as soon as the handler returns, and a terminated transaction can no longer carry the 200 OK of an answer that arrives when the user finally picks up")
	case <-time.After(250 * time.Millisecond):
	}

	if err := recorder.Receive(testCancelRequest(invite, "")); err != nil {
		t.Fatalf("Receive(CANCEL) error = %v", err)
	}

	waitClosed(t, returned, "onInvite() did not return after the CANCEL")
}

func TestStreamDialerOnByeEndsARingingInboundCall(t *testing.T) {
	handler := newRingingInboundHandler()
	dialer := testInboundDialer(t, handler)
	invite := testInviteRequest()
	recorder := siptest.NewServerTxRecorder(invite)

	returned := make(chan struct{})

	go func() {
		defer close(returned)

		dialer.onInvite(invite, recorder)
	}()

	session := serverSessionOf(t, waitRinging(t, handler))

	bye := testByeRequest(session)
	dialer.onBye(bye, siptest.NewServerTxRecorder(bye))

	// onBye reports the end of a ringing call synchronously, so there is
	// nothing to wait for here.
	select {
	case reason := <-handler.ended:
		if reason != core.CallEndReasonCancelled {
			t.Fatalf("EndIncoming reason = %q, want %q", reason, core.CallEndReasonCancelled)
		}
	default:
		t.Fatal("the BYE never reached EndIncoming, so the pending call would survive until the manager's expiry while every stream request is refused")
	}

	waitClosed(t, returned, "onInvite() did not return after the BYE")

	if inboundSessionCached(t, dialer, session) {
		t.Fatal("the dialog a BYE ended is still in sipgo's server cache")
	}
}

// TestStreamDialerOnByeEndsAnAnsweredInboundCall pins the answered branch of
// onInboundBye, which is a different call than the ringing one above.
//
// A BYE for a call that was answered here ends an *active* dialog, and clearing
// that is Manager.RemoteDialogEnded's job, not EndIncoming's. Without it the
// manager goes on believing a call is up, and StartStream deliberately returns
// nil without dialling while one is: every later preview would then succeed
// while sending no INVITE, giving a permanently black stream with no error
// anywhere.
func TestStreamDialerOnByeEndsAnAnsweredInboundCall(t *testing.T) {
	handler := newRingingInboundHandler()
	dialer := testInboundDialer(t, handler)
	invite := testInviteRequest()
	recorder := siptest.NewServerTxRecorder(invite)

	returned := make(chan struct{})

	go func() {
		defer close(returned)

		dialer.onInvite(invite, recorder)
	}()

	dialog := waitRinging(t, handler)
	session := serverSessionOf(t, dialog)

	answered := make(chan error, 1)

	go func() { answered <- dialog.Respond(context.Background(), 200, "OK", "v=0\r\n") }()

	waitDialogState(t, session, sip.DialogStateEstablished)

	ack := testAckRequest(session)
	dialer.onAck(ack, siptest.NewServerTxRecorder(ack))

	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("Respond(200) error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the answer never completed after the ACK")
	}

	bye := testByeRequest(session)
	dialer.onBye(bye, siptest.NewServerTxRecorder(bye))

	// onBye reports the end of the dialog synchronously, so there is nothing to
	// wait for here.
	select {
	case <-handler.remoteEnded:
	default:
		t.Fatal("the BYE for an answered call never reached RemoteDialogEnded: the manager keeps an active dialog the far end has already torn down, and every later preview returns nil without sending an INVITE")
	}

	select {
	case reason := <-handler.ended:
		t.Fatalf("EndIncoming(%q) was called for a call that had already been answered: it clears a *pending* call, so it would drop an unrelated inbound call instead", reason)
	default:
	}

	waitClosed(t, returned, "onInvite() did not return after the BYE")
}

// racedCancelTx accepts the cancel hook sipgo's ReadInvite registers and refuses
// the one the adapter registers right after it, which is exactly what a CANCEL
// landing in that window does: the transaction is already Completed, and
// ServerTx.OnCancel answers false because tx.Err() is ErrTransactionCanceled.
type racedCancelTx struct {
	mu        sync.Mutex
	hooks     int
	responses []*sip.Response

	done chan struct{}
	acks chan *sip.Request
}

func newRacedCancelTx() *racedCancelTx {
	return &racedCancelTx{done: make(chan struct{}), acks: make(chan *sip.Request)}
}

func (tx *racedCancelTx) Terminate() {}

func (tx *racedCancelTx) OnTerminate(sip.FnTxTerminate) bool { return true }

func (tx *racedCancelTx) Done() <-chan struct{} { return tx.done }

func (tx *racedCancelTx) Err() error { return sip.ErrTransactionCanceled }

func (tx *racedCancelTx) Acks() <-chan *sip.Request { return tx.acks }

func (tx *racedCancelTx) Respond(res *sip.Response) error {
	tx.mu.Lock()
	tx.responses = append(tx.responses, res)
	tx.mu.Unlock()

	return nil
}

func (tx *racedCancelTx) OnCancel(sip.FnTxCancel) bool {
	tx.mu.Lock()
	defer tx.mu.Unlock()

	tx.hooks++

	return tx.hooks == 1
}

func TestStreamDialerOnInviteDropsAnAlreadyCancelledInvite(t *testing.T) {
	handler := newRingingInboundHandler()
	dialer := testInboundDialer(t, handler)
	invite := testInviteRequest()

	returned := make(chan struct{})

	go func() {
		defer close(returned)

		dialer.onInvite(invite, newRacedCancelTx())
	}()

	waitClosed(t, returned, "onInvite() did not return for an INVITE whose transaction was already cancelled")

	if handler.invited() != 0 {
		t.Fatal("the manager was told about an INVITE that was already dead: it reserves the slot, publishes IncomingCallStarted and then rolls back, which reports the call as cancelled even when the CANCEL said it was answered elsewhere")
	}

	select {
	case reason := <-handler.ended:
		t.Fatalf("EndIncoming(%q) was called for a dialog the manager was never told about", reason)
	default:
	}

	// The dialog reached Ended before the OnState hook was registered, so that
	// hook never fires for it. The teardown's close branch is what has to
	// delete it instead; a stale key here can never be matched by a later
	// call, so a miss here leaks for the life of the process.
	if count := inboundDialogCount(dialer); count != 0 {
		t.Fatalf("inbound dialog map has %d entries after an already-cancelled INVITE, want 0", count)
	}
}

func TestStreamDialerCloseWaitsForInboundHandlers(t *testing.T) {
	handler := newRingingInboundHandler()
	dialer := testInboundDialer(t, handler)
	invite := testInviteRequest()
	recorder := siptest.NewServerTxRecorder(invite)

	returned := make(chan struct{})

	go func() {
		defer close(returned)

		dialer.onInvite(invite, recorder)
	}()

	waitRinging(t, handler)

	closed := make(chan struct{})

	go func() {
		defer close(closed)

		_ = dialer.Close()
	}()

	select {
	case <-closed:
		t.Fatal("Close() returned while a goroutine was still inside the inbound handler, free to publish events into the manager")
	case <-time.After(150 * time.Millisecond):
	}

	// Whatever wakes the handler — here the transaction being terminated by
	// hand, in production ua.Close tearing the transaction layer down — Close
	// must not return before it has.
	recorder.Terminate()

	waitClosed(t, returned, "onInvite() did not return once its transaction was terminated")
	waitClosed(t, closed, "Close() did not return once the inbound handler had finished")
}

package signaling

import (
	"bticino-go-companion/internal/core"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

var (
	ErrNoIncomingDialog = errors.New("sip: no incoming dialog")
	ErrIncomingDialog   = errors.New("sip: an incoming dialog exists")
	ErrActiveDialog     = errors.New("sip: an active dialog exists")
	ErrNoInboundCall    = errors.New("sip: no inbound call to adopt")
)

const defaultIncomingTimeout = 60 * time.Second

type IncomingDialog interface {
	ID() core.DialogID
	Respond(context.Context, int, string, string) error
	Bye(context.Context) error
}

type OutgoingDialog interface {
	Bye(context.Context) error
}

type StreamDialer interface {
	StartStream(context.Context, string, string) (OutgoingDialog, error)
}

type EventSink interface {
	Publish(core.Event)
}

// EntrypointResolver attributes an inbound call to a configured entrypoint and
// returns its devaddr. An empty ID means the call cannot be attributed.
//
// It is invoked with the Manager's state lock held, so an implementation must be
// a side-effect-free lookup: it must not block, must not do I/O, and must never
// call back into the Manager, which would deadlock on that lock.
type EntrypointResolver func() (core.EntrypointID, string)

// The manager is the dialer's inbound handler. Asserting it here is what keeps
// a new lifecycle callback on InboundHandler from being added without the
// manager growing an implementation for it.
var _ InboundHandler = (*Manager)(nil)

// Manager owns the single inbound and the single outbound SIP dialog. It is the
// only component that knows whether a real dialog exists.
type Manager struct {
	// mu guards every field below, including the publish queue.
	mu sync.Mutex

	// pending holds committed events waiting for the sink, and draining says a
	// drain loop already owns them. The invariant is: an event is appended under
	// mu at the exact point its transition commits, so the queue order is commit
	// order; exactly one goroutine drains at a time, and it calls the sink with
	// mu released. A publisher that finds a drain already running hands its
	// event over and returns immediately; the one that finds none starts a drain
	// goroutine and returns too, so no caller ever waits for the sink. That gives
	// commit-ordered, never overlapping deliveries without ever holding a lock —
	// or a SIP entry point — across the sink. The sink is external code (it
	// broadcasts to WebSocket clients synchronously, each write with its own
	// deadline) and may even call back into the Manager, which simply enqueues.
	pending  []core.Event
	draining bool

	host    string
	dialer  StreamDialer
	events  EventSink
	resolve EntrypointResolver

	incoming        IncomingDialog
	incomingDevAddr string
	incomingExpiry  *time.Timer
	incomingTimeout time.Duration

	active         OutgoingDialog
	activeID       core.DialogID
	activeIncoming bool
	// activeDevAddr is the entrypoint the answered inbound call belongs to, kept
	// because Answer clears incomingDevAddr.
	activeDevAddr string
}

func NewManager(host string, dialer StreamDialer, events EventSink, resolve EntrypointResolver) *Manager {
	if resolve == nil {
		resolve = func() (core.EntrypointID, string) { return "", "" }
	}

	return &Manager{host: host, dialer: dialer, events: events, resolve: resolve, incomingTimeout: defaultIncomingTimeout}
}

// SetEvents assigns the sink after construction, because the projector-backed
// applier is not available when the manager is created.
//
// Events published while the sink is still nil are dropped, not buffered: the
// queue exists to order deliveries, not to hold them for a sink that does not
// exist yet. The sink must therefore be installed before the SIP listener can
// deliver an INVITE, or the call that arrives in between never reaches the
// projector while the manager believes it is ringing.
func (m *Manager) SetEvents(events EventSink) {
	m.mu.Lock()
	m.events = events
	m.mu.Unlock()
}

// SetIncomingTimeout overrides how long an unanswered inbound call is kept.
func (m *Manager) SetIncomingTimeout(timeout time.Duration) {
	m.mu.Lock()
	m.incomingTimeout = timeout
	m.mu.Unlock()
}

func (m *Manager) OnInvite(ctx context.Context, dialog IncomingDialog) error {
	m.mu.Lock()

	if m.incoming != nil || m.active != nil {
		m.mu.Unlock()

		return m.respondBusy(ctx, dialog)
	}

	entrypointID, devAddr := m.resolve()
	if entrypointID == "" {
		m.mu.Unlock()

		return m.respondBusy(ctx, dialog)
	}

	// Reserve the incoming slot before the 180 goes out: the busy check above
	// and this reservation have to be a single operation, or a second INVITE —
	// or a concurrent StartStream — slips in between them and one of the two
	// dialogs is left with nobody holding a reference to it.
	//
	// The start event is published from inside that same critical section, at
	// the commit point, exactly as Decline and Hangup do. Publishing it after
	// the 180 instead would leave a window in which the expiry, a CANCEL
	// answered elsewhere, a Decline or a Hangup commits first: the projector
	// would then see IncomingCallEnded for a call it has never seen, drop it,
	// apply the late IncomingCallStarted, and stay stuck in "ringing" for good,
	// rejecting every later inbound call.
	m.incoming = dialog
	m.incomingDevAddr = devAddr
	m.startIncomingExpiryLocked(dialog)
	m.publishLocked(core.IncomingCallStarted{DialogID: dialog.ID(), EntrypointID: entrypointID})

	if err := dialog.Respond(ctx, 180, "Ringing", ""); err != nil {
		// The call was already announced, so the rollback has to end it too, or
		// the projector keeps a call that never rang. clearIncoming publishes
		// only if this dialog is still the reserved one, so a publisher that got
		// there first is not doubled up.
		//
		// Its answer is also what says whether this rollback may still speak for
		// the dialog. A failing 180 is slow, and an Answer, a Decline or the
		// expiry can take the dialog over while it is failing — each of them
		// puts its own final response on the transaction. Sending the 500
		// anyway would be a second final response on a transaction this call no
		// longer owns: a 500 stamped on top of a 200 OK, or after the expiry's
		// 480.
		if m.clearIncoming(dialog, core.CallEndReasonCancelled) {
			// Best effort: without a final response the INVITE has had no answer
			// at all and the far end sits out its whole transaction timeout. It
			// gets a fresh context, because the one whose 180 just failed may
			// well have failed it by being cancelled or expired.
			respondCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			_ = dialog.Respond(respondCtx, 500, "Server Error", "")
		}

		return fmt.Errorf("sip ringing response: %w", err)
	}

	return nil
}

func (m *Manager) respondBusy(ctx context.Context, dialog IncomingDialog) error {
	if err := dialog.Respond(ctx, 486, "Busy Here", ""); err != nil {
		return fmt.Errorf("sip busy response: %w", err)
	}

	return nil
}

func (m *Manager) Answer(ctx context.Context) error {
	m.mu.Lock()

	dialog, devAddr := m.incoming, m.incomingDevAddr
	if dialog == nil {
		m.mu.Unlock()

		return ErrNoIncomingDialog
	}

	m.mu.Unlock()

	if err := dialog.Respond(ctx, 200, "OK", BuildAnswer(m.host, devAddr)); err != nil {
		return fmt.Errorf("sip answer response: %w", err)
	}

	m.mu.Lock()

	if m.incoming != dialog {
		// A concurrent Answer — a double tap on the card, or a retried request —
		// may have promoted this very dialog while both were on the wire with
		// their 200 OK. The call is live and referenced; BYEing it here would
		// tear down the call that was just answered.
		if m.active == dialog {
			m.mu.Unlock()

			return nil
		}

		m.mu.Unlock()

		// The dialog is neither the reserved incoming one nor the active one, so
		// as of this critical section no field of the Manager refers to it — and
		// the far end has just been told 200 OK. Nothing else will ever BYE it,
		// so this call does.
		//
		// That is the whole invariant: the loser BYEs only a dialog it can prove
		// is unreferenced. It is not a guarantee that the dialog is BYEd exactly
		// once. The winner may have promoted it and a Hangup may already have
		// torn it down and dropped it from m.active before this goroutine got
		// back onto the lock, in which case the BYE below is a second one. A
		// redundant BYE on a dialog that is already closed is tolerated —
		// leaving a live call with nobody able to end it is not.
		byeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = dialog.Bye(byeCtx)

		return ErrNoIncomingDialog
	}

	m.stopIncomingExpiryLocked()
	m.incoming = nil
	m.incomingDevAddr = ""
	m.active = dialog
	m.activeID = dialog.ID()
	m.activeIncoming = true
	m.activeDevAddr = devAddr
	m.publishLocked(core.CallAnswered{DialogID: dialog.ID()})

	return nil
}

// Decline is retained for the concurrent-call path and is deliberately not
// exposed over HTTP.
func (m *Manager) Decline(ctx context.Context) error {
	m.mu.Lock()

	dialog := m.takeIncomingLocked(nil)
	if dialog == nil {
		m.mu.Unlock()

		return ErrNoIncomingDialog
	}

	m.publishLocked(core.CallDeclined{DialogID: dialog.ID()})

	if err := dialog.Respond(ctx, 603, "Decline", ""); err != nil {
		return fmt.Errorf("sip decline response: %w", err)
	}

	return nil
}

// Hangup is idempotent: tearing down a call that is already gone is not an
// error.
//
// It ends whatever call is up, a ringing one included, which it answers 603
// Decline. That is right for POST /api/v3/call/hangup, where the user is
// rejecting the call, and wrong for anything a media teardown does: a 6xx ends
// every fork of the doorbell call, the indoor unit's included. The media path
// therefore uses HangupCall, never this. See
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
func (m *Manager) Hangup(ctx context.Context) error {
	m.mu.Lock()

	if m.active != nil {
		return m.byeActiveLocked(ctx)
	}

	dialog := m.takeIncomingLocked(nil)
	if dialog == nil {
		m.mu.Unlock()

		return nil
	}

	m.publishLocked(core.CallHungUp{DialogID: dialog.ID()})

	if err := dialog.Respond(ctx, 603, "Decline", ""); err != nil {
		return fmt.Errorf("sip decline response: %w", err)
	}

	return nil
}

// HangupCall ends the active dialog with a BYE and does nothing else: a call
// that is only ringing, or no call at all, is left exactly as it is.
//
// It is the only hangup a media session can reach. A session's teardown —
// whether a consumer let go of the lease or the media failed — must never
// reject a ringing doorbell call, because the 603 would end every fork,
// including the indoor unit that is still ringing. Releasing the lease of an
// answered call still ends it on purpose: without a WebRTC session there is no
// audio path and the visitor would talk to nobody. See
// docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md and
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
func (m *Manager) HangupCall(ctx context.Context) error {
	m.mu.Lock()

	if m.active == nil {
		m.mu.Unlock()

		return nil
	}

	return m.byeActiveLocked(ctx)
}

// byeActiveLocked drops the active dialog and sends its BYE. m.mu must be held
// and m.active must be set; it releases m.mu, like publishLocked.
func (m *Manager) byeActiveLocked(ctx context.Context) error {
	active := m.active
	dialogID := m.activeID
	m.active = nil
	m.activeID = ""
	m.activeIncoming = false
	m.activeDevAddr = ""

	// activeID is empty exactly for an outbound preview, because StartStream
	// publishes nothing: the projector never saw a dialog, and
	// CallHungUp{DialogID: ""} would be rejected as an invalid transition.
	var event core.Event
	if dialogID != "" {
		event = core.CallHungUp{DialogID: dialogID}
	}

	m.publishLocked(event)

	if err := active.Bye(ctx); err != nil {
		return fmt.Errorf("sip bye: %w", err)
	}

	return nil
}

// RemoteDialogEnded clears the active dialog after the far end has terminated
// it. It sends no BYE, because the peer already did, and is a no-op when
// nothing is active — without it the manager would keep believing a call is up
// and every later preview would succeed without sending an INVITE.
func (m *Manager) RemoteDialogEnded() {
	m.mu.Lock()

	if m.active == nil {
		m.mu.Unlock()

		return
	}

	dialogID := m.activeID
	m.active = nil
	m.activeID = ""
	m.activeIncoming = false
	m.activeDevAddr = ""

	// See Hangup: an empty dialog ID is an outbound preview, which the
	// projector knows nothing about.
	var event core.Event
	if dialogID != "" {
		event = core.CallHungUp{DialogID: dialogID}
	}

	m.publishLocked(event)
}

// HasAnsweredInboundCall reports whether the active dialog is an inbound call
// the companion has answered.
//
// The media coordinator asks this before refusing a lease for a stream it sees
// as externally owned. The intercom starts its AV while the call is still
// ringing, so the stream is already marked external by the time the user
// answers; without this the answered call could never obtain a lease and the
// card would never show video. See
// docs/superpowers/specs/2026-08-02-inbound-call-stream-ownership-design.md.
func (m *Manager) HasAnsweredInboundCall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.active != nil && m.activeIncoming
}

// HasRingingInboundCall reports whether an inbound call is ringing and not yet
// answered here. The media coordinator asks it to decide which consumers may
// take the stream during the ring: only a ringing preview may.
func (m *Manager) HasRingingInboundCall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.incoming != nil
}

// AdoptRingingCall lets a ringing preview borrow the inbound call's stream
// without dialling. It reports whether the call has already been answered,
// which tells the session to start audio too.
//
// It never creates a dialog, so the session that calls it never owns one: no
// teardown of its media can end the call. ErrNoInboundCall means the ring ended
// before the preview got here; a fresh INVITE would only come back 486 Busy
// Here from an intercom still winding the call down. See
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
//
// devAddr is the entrypoint the preview is for. Another entrypoint's ring is
// not its call to borrow: on an install with several, its preview would show
// the wrong door.
func (m *Manager) AdoptRingingCall(devAddr string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch {
	case m.incoming != nil && devAddr == m.incomingDevAddr:
		return false, nil
	case m.active != nil && m.activeIncoming && devAddr == m.activeDevAddr:
		return true, nil
	default:
		return false, ErrNoInboundCall
	}
}

// EndIncoming clears a pending inbound call that will never be answered here.
func (m *Manager) EndIncoming(reason core.CallEndReason) {
	// The core layer does not validate the reason, so neither a bare nor an
	// unknown one may leave the manager.
	switch reason {
	case core.CallEndReasonCancelled, core.CallEndReasonTimeout, core.CallEndReasonElsewhere:
	default:
		reason = core.CallEndReasonCancelled
	}

	m.clearIncoming(nil, reason)
}

func (m *Manager) clearIncoming(expected IncomingDialog, reason core.CallEndReason) bool {
	m.mu.Lock()

	dialog := m.takeIncomingLocked(expected)
	if dialog == nil {
		m.mu.Unlock()

		return false
	}

	m.publishLocked(core.IncomingCallEnded{DialogID: dialog.ID(), Reason: reason})

	return true
}

// takeIncomingLocked detaches the pending inbound dialog and disarms its expiry,
// provided it is still the expected one. m.mu must be held.
func (m *Manager) takeIncomingLocked(expected IncomingDialog) IncomingDialog {
	dialog := m.incoming
	if dialog == nil || (expected != nil && dialog != expected) {
		return nil
	}

	m.stopIncomingExpiryLocked()
	m.incoming = nil
	m.incomingDevAddr = ""

	return dialog
}

func (m *Manager) startIncomingExpiryLocked(dialog IncomingDialog) {
	m.stopIncomingExpiryLocked()

	timeout := m.incomingTimeout
	if timeout <= 0 {
		timeout = defaultIncomingTimeout
	}

	m.incomingExpiry = time.AfterFunc(timeout, func() {
		if !m.clearIncoming(dialog, core.CallEndReasonTimeout) {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = dialog.Respond(ctx, 480, "Temporarily Unavailable", "")
	})
}

func (m *Manager) stopIncomingExpiryLocked() {
	if m.incomingExpiry != nil {
		m.incomingExpiry.Stop()
		m.incomingExpiry = nil
	}
}

// StartStream dials the intercom and reports whether this call created the SIP
// dialog.
//
// It reports false when an answered inbound call is already up: the intercom is
// streaming for that call, no INVITE goes out, and the dialog belongs to the
// call rather than to the media session that asked. media.SourceSession needs
// that distinction to know whether a BYE of its own is ever appropriate — a
// teardown of its media must not end a call the user is on. See
// docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md.
func (m *Manager) StartStream(ctx context.Context, devAddr string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.incoming != nil {
		return false, ErrIncomingDialog
	}

	// The intercom is already streaming for the answered call; a second INVITE
	// would come back as 486 Busy Here.
	if m.activeIncoming {
		return false, nil
	}

	if m.active != nil {
		return false, ErrActiveDialog
	}

	dialog, err := m.dialer.StartStream(ctx, devAddr, BuildOffer(m.host, devAddr))
	if err != nil {
		return false, fmt.Errorf("outgoing stream: %w", err)
	}

	m.active = dialog

	return true, nil
}

// publishLocked hands a committed state transition to the sink. It must be
// called with m.mu held and it releases it, so callers must not defer
// m.mu.Unlock() and must not touch guarded state afterwards.
//
// The event is queued under m.mu, at the commit point, and delivered by a drain
// goroutine — see the pending/draining fields. No publisher ever waits for the
// sink: one that finds a drain already running enqueues and returns, and one
// that finds none starts the drain and returns. That matters most in OnInvite,
// which publishes IncomingCallStarted before the 180 Ringing: running the sink
// here would put a broadcast to every WebSocket client, each write with its own
// deadline, in front of the provisional response, and the intercom gives up on
// an INVITE it never sees ringing.
//
// A nil event releases the lock without publishing, which keeps the branches
// that commit without telling the projector on the same single unlock path as
// the ones that do.
func (m *Manager) publishLocked(event core.Event) {
	if event != nil {
		m.pending = append(m.pending, event)
	}

	if m.draining || len(m.pending) == 0 {
		m.mu.Unlock()

		return
	}

	// draining is claimed under m.mu, before the goroutine exists. That is what
	// keeps a single drainer — and therefore commit order — no matter how many
	// publishers race here.
	m.draining = true
	m.mu.Unlock()

	go m.drainEvents()
}

// drainEvents delivers queued events in commit order, one at a time, with m.mu
// released across every call into the sink.
func (m *Manager) drainEvents() {
	// The loop hands the drain back under the same lock that finds the queue
	// empty, so a publisher cannot enqueue into a drain that is about to stop.
	// The defer covers the abnormal exit instead: without it, a panic on this
	// goroutine would leave draining set for good, every later publish would
	// queue behind a drain that no longer exists, and nothing would ever reach
	// the projector again — silently, until a restart.
	handedBack := false

	defer func() {
		if handedBack {
			return
		}

		m.mu.Lock()
		m.draining = false
		m.mu.Unlock()
	}()

	for {
		m.mu.Lock()

		if len(m.pending) == 0 {
			m.draining = false
			handedBack = true
			m.mu.Unlock()

			return
		}

		event := m.pending[0]

		m.pending = m.pending[1:]
		if len(m.pending) == 0 {
			m.pending = nil
		}

		events := m.events
		m.mu.Unlock()

		if events != nil {
			deliver(events, event)
		}
	}
}

// deliver calls the sink and contains its panics. The sink is external code, so
// one bad event must neither stop the drain — which would strand every later
// event behind it — nor take the process down with it, which is what an
// unrecovered panic on the drain's own goroutine would do.
func deliver(events EventSink, event core.Event) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Default().Error("signaling: event sink panicked",
				"component", "signaling.manager",
				"event", fmt.Sprintf("%T", event),
				"panic", recovered,
			)
		}
	}()

	events.Publish(event)
}

package signaling

import (
	"bticino-go-companion/internal/core"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrNoIncomingDialog = errors.New("sip: no incoming dialog")
	ErrIncomingDialog   = errors.New("sip: an incoming dialog exists")
	ErrActiveDialog     = errors.New("sip: an active dialog exists")
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
type EntrypointResolver func() (core.EntrypointID, string)

// Manager owns the single inbound and the single outbound SIP dialog. It is the
// only component that knows whether a real dialog exists.
type Manager struct {
	// mu guards the dialog state below. publishMu serializes deliveries to the
	// sink so they reach it in the order their transitions committed: a
	// publisher takes publishMu while it still holds mu, drops mu, and only
	// then calls the sink, which is external code and must never run under the
	// state lock. Lock ordering is therefore mu then publishMu, never the
	// reverse — nothing may take mu while holding publishMu.
	mu        sync.Mutex
	publishMu sync.Mutex

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
}

func NewManager(host string, dialer StreamDialer, events EventSink, resolve EntrypointResolver) *Manager {
	if resolve == nil {
		resolve = func() (core.EntrypointID, string) { return "", "" }
	}

	return &Manager{host: host, dialer: dialer, events: events, resolve: resolve, incomingTimeout: defaultIncomingTimeout}
}

// SetEvents assigns the sink after construction, because the projector-backed
// applier is not available when the manager is created.
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
	m.incoming = dialog
	m.incomingDevAddr = devAddr
	m.startIncomingExpiryLocked(dialog)
	m.mu.Unlock()

	if err := dialog.Respond(ctx, 180, "Ringing", ""); err != nil {
		// The call never started, so the reservation is rolled back silently:
		// publishing IncomingCallEnded here would end a call nobody was told
		// about.
		m.releaseIncoming(dialog)

		return fmt.Errorf("sip ringing response: %w", err)
	}

	m.mu.Lock()
	m.publishLocked(core.IncomingCallStarted{DialogID: dialog.ID(), EntrypointID: entrypointID})

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
		m.mu.Unlock()

		// The expiry timer — or a Decline — cleared the slot while the 200 OK
		// was on the wire. The far end is in a live call and no field of the
		// Manager refers to this dialog any more, so it has to be ended here or
		// it survives with nothing able to BYE it.
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
// error, because SourceSession.Close runs it again on every normal teardown.
func (m *Manager) Hangup(ctx context.Context) error {
	m.mu.Lock()

	if active := m.active; active != nil {
		dialogID := m.activeID
		m.active = nil
		m.activeID = ""
		m.activeIncoming = false

		// activeID is empty exactly for an outbound preview, because
		// StartStream publishes nothing: the projector never saw a dialog, and
		// CallHungUp{DialogID: ""} would be rejected as an invalid transition.
		// The guard discriminates preview teardown from a real call.
		if dialogID != "" {
			m.publishLocked(core.CallHungUp{DialogID: dialogID})
		} else {
			m.mu.Unlock()
		}

		if err := active.Bye(ctx); err != nil {
			return fmt.Errorf("sip bye: %w", err)
		}

		return nil
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

	// See Hangup: an empty dialog ID is an outbound preview, which the
	// projector knows nothing about.
	if dialogID == "" {
		m.mu.Unlock()

		return
	}

	m.publishLocked(core.CallHungUp{DialogID: dialogID})
}

// EndIncoming clears a pending inbound call that will never be answered here.
func (m *Manager) EndIncoming(reason core.CallEndReason) {
	// The core layer does not validate the reason, so a bare one must never
	// leave the manager.
	if reason == "" {
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

// releaseIncoming detaches a reserved inbound dialog without publishing: it is
// the rollback for a call that never made it to ringing.
func (m *Manager) releaseIncoming(expected IncomingDialog) {
	m.mu.Lock()
	m.takeIncomingLocked(expected)
	m.mu.Unlock()
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

func (m *Manager) StartStream(ctx context.Context, devAddr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.incoming != nil {
		return ErrIncomingDialog
	}

	// The intercom is already streaming for the answered call; a second INVITE
	// would come back as 486 Busy Here.
	if m.activeIncoming {
		return nil
	}

	if m.active != nil {
		return ErrActiveDialog
	}

	dialog, err := m.dialer.StartStream(ctx, devAddr, BuildOffer(m.host, devAddr))
	if err != nil {
		return err
	}

	m.active = dialog

	return nil
}

// publishLocked hands a committed state transition to the sink. It must be
// called with m.mu held and it releases it: it takes publishMu first, so that
// deliveries are serialized in the order the transitions committed, then drops
// m.mu so the sink never runs under the state lock. Callers must not defer
// m.mu.Unlock() and must not touch guarded state after calling it.
func (m *Manager) publishLocked(event core.Event) {
	m.publishMu.Lock()
	defer m.publishMu.Unlock()

	events := m.events
	m.mu.Unlock()

	if events != nil {
		events.Publish(event)
	}
}

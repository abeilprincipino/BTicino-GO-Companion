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
	mu sync.Mutex

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

		return dialog.Respond(ctx, 486, "Busy Here", "")
	}

	entrypointID, devAddr := m.resolve()
	if entrypointID == "" {
		m.mu.Unlock()

		return dialog.Respond(ctx, 486, "Busy Here", "")
	}

	m.mu.Unlock()

	if err := dialog.Respond(ctx, 180, "Ringing", ""); err != nil {
		return err
	}

	m.mu.Lock()
	m.incoming = dialog
	m.incomingDevAddr = devAddr
	m.startIncomingExpiryLocked(dialog)
	m.mu.Unlock()

	m.publish(core.IncomingCallStarted{DialogID: dialog.ID(), EntrypointID: entrypointID})

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
		return err
	}

	m.mu.Lock()

	if m.incoming != dialog {
		m.mu.Unlock()

		return ErrNoIncomingDialog
	}

	m.stopIncomingExpiryLocked()
	m.incoming = nil
	m.incomingDevAddr = ""
	m.active = dialog
	m.activeID = dialog.ID()
	m.activeIncoming = true
	m.mu.Unlock()

	m.publish(core.CallAnswered{DialogID: dialog.ID()})

	return nil
}

// Decline is retained for the concurrent-call path and is deliberately not
// exposed over HTTP.
func (m *Manager) Decline(ctx context.Context) error {
	m.mu.Lock()

	dialog := m.incoming
	if dialog == nil {
		m.mu.Unlock()

		return ErrNoIncomingDialog
	}

	m.stopIncomingExpiryLocked()
	m.incoming = nil
	m.incomingDevAddr = ""
	m.mu.Unlock()

	if err := dialog.Respond(ctx, 603, "Decline", ""); err != nil {
		return err
	}

	m.publish(core.CallDeclined{DialogID: dialog.ID()})

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
		m.mu.Unlock()

		err := active.Bye(ctx)
		if dialogID != "" {
			m.publish(core.CallHungUp{DialogID: dialogID})
		}

		if err != nil {
			return fmt.Errorf("sip bye: %w", err)
		}

		return nil
	}

	dialog := m.incoming
	if dialog == nil {
		m.mu.Unlock()

		return nil
	}

	m.stopIncomingExpiryLocked()
	m.incoming = nil
	m.incomingDevAddr = ""
	m.mu.Unlock()

	if err := dialog.Respond(ctx, 603, "Decline", ""); err != nil {
		return err
	}

	m.publish(core.CallHungUp{DialogID: dialog.ID()})

	return nil
}

// EndIncoming clears a pending inbound call that will never be answered here.
func (m *Manager) EndIncoming(reason core.CallEndReason) {
	m.clearIncoming(nil, reason)
}

func (m *Manager) clearIncoming(expected IncomingDialog, reason core.CallEndReason) bool {
	m.mu.Lock()

	dialog := m.incoming
	if dialog == nil || (expected != nil && dialog != expected) {
		m.mu.Unlock()

		return false
	}

	m.stopIncomingExpiryLocked()
	m.incoming = nil
	m.incomingDevAddr = ""
	m.mu.Unlock()

	m.publish(core.IncomingCallEnded{DialogID: dialog.ID(), Reason: reason})

	return true
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

func (m *Manager) publish(event core.Event) {
	m.mu.Lock()
	events := m.events
	m.mu.Unlock()

	if events != nil {
		events.Publish(event)
	}
}

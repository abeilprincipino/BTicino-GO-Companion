package sip

import (
	"context"
	"errors"
	"sync"

	"bticino-go-companion/internal/core"
)

var (
	ErrNoIncomingDialog = errors.New("no incoming dialog")
	ErrIncomingDialog   = errors.New("an incoming dialog exists")
	ErrNoActiveDialog   = errors.New("no active dialog")
)

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

type Manager struct {
	mu sync.Mutex

	host   string
	dialer StreamDialer
	events EventSink

	incoming IncomingDialog
	active   interface{ Bye(context.Context) error }
}

func NewManager(host string, dialer StreamDialer, events EventSink) *Manager {
	return &Manager{host: host, dialer: dialer, events: events}
}

func (m *Manager) OnInvite(ctx context.Context, dialog IncomingDialog, entrypointID core.EntrypointID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.incoming != nil {
		return ErrIncomingDialog
	}
	if err := dialog.Respond(ctx, 180, "Ringing", ""); err != nil {
		return err
	}
	m.incoming = dialog
	m.publish(core.IncomingCallStarted{DialogID: dialog.ID(), EntrypointID: entrypointID})
	return nil
}

func (m *Manager) Answer(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.incoming == nil {
		return ErrNoIncomingDialog
	}
	if err := m.incoming.Respond(ctx, 200, "OK", BuildAnswer(m.host)); err != nil {
		return err
	}
	dialog := m.incoming
	m.incoming = nil
	m.active = dialog
	m.publish(core.CallAnswered{DialogID: dialog.ID()})
	return nil
}

func (m *Manager) Decline(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.incoming == nil {
		return ErrNoIncomingDialog
	}
	if err := m.incoming.Respond(ctx, 603, "Decline", ""); err != nil {
		return err
	}
	dialog := m.incoming
	m.incoming = nil
	m.publish(core.CallDeclined{DialogID: dialog.ID()})
	return nil
}

func (m *Manager) Hangup(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.active != nil {
		if err := m.active.Bye(ctx); err != nil {
			return err
		}
		m.active = nil
		return nil
	}
	if m.incoming == nil {
		return ErrNoActiveDialog
	}
	if err := m.incoming.Respond(ctx, 603, "Decline", ""); err != nil {
		return err
	}
	dialog := m.incoming
	m.incoming = nil
	m.publish(core.CallHungUp{DialogID: dialog.ID()})
	return nil
}

func (m *Manager) StartStream(ctx context.Context, devAddr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.incoming != nil {
		return ErrIncomingDialog
	}
	dialog, err := m.dialer.StartStream(ctx, devAddr, BuildOffer(m.host, devAddr))
	if err != nil {
		return err
	}
	m.active = dialog
	return nil
}

func (m *Manager) publish(event core.Event) {
	if m.events != nil {
		m.events.Publish(event)
	}
}

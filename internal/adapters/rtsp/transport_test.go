package rtspadapter

import (
	"context"
	"testing"
)

type lifecycleStub struct {
	joinCalls   int
	leaveCalls  int
	touchCalls  int
	lastSession string
	lastEntryp  string
	lastDevAddr string
}

func (l *lifecycleStub) ReaderJoin(_ context.Context, sessionID string, entrypointID string, devAddr string) error {
	l.joinCalls++
	l.lastSession = sessionID
	l.lastEntryp = entrypointID
	l.lastDevAddr = devAddr
	return nil
}

func (l *lifecycleStub) ReaderLeave(_ context.Context, sessionID string) error {
	l.leaveCalls++
	l.lastSession = sessionID
	return nil
}

func (l *lifecycleStub) ReaderTouch(sessionID string) {
	l.touchCalls++
	l.lastSession = sessionID
}

func TestTransportDelegatesLifecycle(t *testing.T) {
	lifecycle := &lifecycleStub{}
	transport := NewTransport(lifecycle)

	if err := transport.OnPlay(context.Background(), "s1", "main", "20"); err != nil {
		t.Fatalf("on play failed: %v", err)
	}
	if err := transport.OnPause(context.Background(), "s1"); err != nil {
		t.Fatalf("on pause failed: %v", err)
	}
	transport.OnGetParameter("s1")
	transport.OnSetParameter("s1")

	if lifecycle.joinCalls != 1 || lifecycle.leaveCalls != 1 || lifecycle.touchCalls != 2 {
		t.Fatalf("unexpected lifecycle counters: %+v", lifecycle)
	}
	if lifecycle.lastEntryp != "main" || lifecycle.lastDevAddr != "20" {
		t.Fatalf("unexpected lifecycle payload: %+v", lifecycle)
	}
}

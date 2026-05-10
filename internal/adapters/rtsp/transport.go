package rtspadapter

import "context"

type Lifecycle interface {
	ReaderJoin(ctx context.Context, sessionID string, entrypointID string, devAddr string) error
	ReaderLeave(ctx context.Context, sessionID string) error
	ReaderTouch(sessionID string)
}

type Transport struct {
	lifecycle Lifecycle
}

func NewTransport(lifecycle Lifecycle) *Transport {
	return &Transport{lifecycle: lifecycle}
}

func (t *Transport) OnPlay(ctx context.Context, sessionID string, entrypointID string, devAddr string) error {
	if t.lifecycle == nil {
		return nil
	}
	return t.lifecycle.ReaderJoin(ctx, sessionID, entrypointID, devAddr)
}

func (t *Transport) OnPause(ctx context.Context, sessionID string) error {
	if t.lifecycle == nil {
		return nil
	}
	return t.lifecycle.ReaderLeave(ctx, sessionID)
}

func (t *Transport) OnGetParameter(sessionID string) {
	if t.lifecycle == nil {
		return
	}
	t.lifecycle.ReaderTouch(sessionID)
}

func (t *Transport) OnSetParameter(sessionID string) {
	if t.lifecycle == nil {
		return
	}
	t.lifecycle.ReaderTouch(sessionID)
}

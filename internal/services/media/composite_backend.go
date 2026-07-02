package media

import (
	"context"
	"errors"
	"log"
	"sync"
)

const (
	callStateRinging = "ringing"
	callStateActive  = "active"
)

type SIPBackend interface {
	StreamStart(ctx context.Context, devAddr string) error
	StreamStop(ctx context.Context) error
}

type StreamCommandBackend interface {
	StreamStart(ctx context.Context, audioPort, videoPort int) error
}

type CompositeBackendOptions struct {
	SIP       SIPBackend
	Commands  StreamCommandBackend // legacy openserver path (used only when SIP is absent)
	AV        StreamCommandBackend // bt_ipcamera AV endpoint; non-nil enables the call-state gate
	CallState func() string        // "idle" | "ringing" | "active"; nil means always idle
	AudioPort int
	VideoPort int
	Logger    *log.Logger
}

type compositeBackend struct {
	opts CompositeBackendOptions

	mu         sync.Mutex
	sipStarted bool
}

func NewCompositeBackend(opts CompositeBackendOptions) Backend {
	return &compositeBackend{opts: opts}
}

func (b *compositeBackend) StreamStart(ctx context.Context, devAddr string) error {
	if b.opts.AV == nil {
		// Legacy path (C300X and default installs): unchanged behaviour.
		if b.opts.SIP != nil {
			return b.opts.SIP.StreamStart(ctx, devAddr)
		}
		if b.opts.Commands != nil {
			return b.opts.Commands.StreamStart(ctx, b.opts.AudioPort, b.opts.VideoPort)
		}
		return nil
	}

	state := b.callState()
	if state == callStateRinging || state == callStateActive {
		// The AV pipeline is already armed by the ongoing call; an INVITE now
		// would only earn us a 486 Busy Here.
		b.logf("media: call state %q — skipping SIP INVITE, AV add-stream only", state)
		return b.opts.AV.StreamStart(ctx, b.opts.AudioPort, b.opts.VideoPort)
	}

	sipStarted := false
	var sipErr error
	if b.opts.SIP != nil {
		sipErr = b.opts.SIP.StreamStart(ctx, devAddr)
		switch {
		case sipErr == nil:
			sipStarted = true
		case errors.Is(sipErr, ErrSIPCallInProgress):
			b.logf("media: call in progress detected via 486 — AV add-stream only")
			sipErr = nil
		default:
			b.logf("media: SIP stream start failed (%v) — attempting AV add-stream anyway", sipErr)
		}
	}

	if avErr := b.opts.AV.StreamStart(ctx, b.opts.AudioPort, b.opts.VideoPort); avErr != nil {
		if sipStarted {
			if stopErr := b.opts.SIP.StreamStop(ctx); stopErr != nil {
				b.logf("media: SIP cleanup after AV failure also failed: %v", stopErr)
			}
		}
		return errors.Join(sipErr, avErr)
	}

	b.mu.Lock()
	b.sipStarted = sipStarted
	b.mu.Unlock()
	return nil
}

func (b *compositeBackend) StreamStop(ctx context.Context) error {
	if b.opts.AV == nil {
		if b.opts.SIP != nil {
			return b.opts.SIP.StreamStop(ctx)
		}
		return nil
	}
	b.mu.Lock()
	started := b.sipStarted
	b.sipStarted = false
	b.mu.Unlock()
	if started && b.opts.SIP != nil {
		return b.opts.SIP.StreamStop(ctx)
	}
	return nil
}

func (b *compositeBackend) callState() string {
	if b.opts.CallState == nil {
		return "idle"
	}
	return b.opts.CallState()
}

func (b *compositeBackend) logf(format string, args ...any) {
	if b.opts.Logger != nil {
		b.opts.Logger.Printf(format, args...)
	}
}

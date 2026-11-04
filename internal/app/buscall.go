package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"bticino-go-companion/internal/openwebnet"
)

// busCallEndTimeout bounds the OpenWebNet command that closes a call.
const busCallEndTimeout = 5 * time.Second

// busCall follows the door-entry call on the bus, so the companion can close it
// when it ends its own SIP leg: the BYE or the 603 ends only that leg, and the
// indoor unit kept the call up until the intercom's own timeout. Sending the
// call end is a beta experiment.
type busCall struct {
	mu       sync.Mutex
	callType string
	called   string
	// open is set by the call start and the answer, and cleared by a call end.
	// The intercom sends a transient call end while a view starts, followed by
	// another answer, so the answer has to open the call again.
	open bool
}

// observeFrame takes every OpenWebNet frame.
func (b *busCall) observeFrame(frame string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if callType, called, ok := openwebnet.ParseCallStart(frame); ok {
		b.callType, b.called, b.open = callType, called, true

		return
	}

	switch {
	case openwebnet.IsCallAnswer(frame):
		b.open = b.called != ""
	case openwebnet.IsCallEnd(frame):
		b.open = false
	}
}

// take returns the call still open on the bus and marks it closed, so a call is
// closed once however many hangups report it.
func (b *busCall) take() (callType, called string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.open {
		return "", "", false
	}

	b.open = false

	return b.callType, b.called, true
}

// endBusCall closes the call still open on the bus, for every device, the
// indoor unit included, and reports whether the bus took the frame. It does
// nothing when the bus has already ended the call.
func endBusCall(ctx context.Context, control *openwebnet.Control, call *busCall, logger *slog.Logger) bool {
	callType, called, ok := call.take()
	if !ok {
		logger.Debug("bus call end skipped; no call open on the bus")
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, busCallEndTimeout)
	defer cancel()

	if err := control.EndCall(ctx, callType, called); err != nil {
		logger.Warn("bus call end failed", "call_type", callType, "called", called, "error", err)
		return false
	}

	logger.Info("bus call end sent", "call_type", callType, "called", called)

	return true
}

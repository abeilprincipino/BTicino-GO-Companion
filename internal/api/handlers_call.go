package api

import (
	"bticino-go-companion/internal/signaling"
	"context"
	"errors"
	"net/http"
	"time"
)

// callControlTimeout bounds how long an HTTP request waits on the SIP layer.
// Answer blocks for the caller's ACK and Hangup blocks for the BYE response;
// both retransmit for up to 32s before giving up on an unresponsive peer, but
// the peer here is the intercom's own SIP stack on localhost, which acks in
// milliseconds under normal operation. A few seconds gives that generous
// headroom without holding the HTTP request (and the button-press UI) open
// anywhere near the full 32s worst case. It also matches the timeouts the
// signaling manager already uses internally for its own localhost responses
// (see signaling.Manager's respondCtx/byeCtx).
const callControlTimeout = 5 * time.Second

func (s *Server) answerCall(w http.ResponseWriter, r *http.Request) {
	if s.call == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "call control is unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), callControlTimeout)
	defer cancel()

	if err := s.call.Answer(ctx); err != nil {
		if errors.Is(err, signaling.ErrNoIncomingDialog) {
			s.logger.InfoContext(r.Context(), "call answer rejected", "reason", "no incoming call")
			writeError(w, http.StatusConflict, "no_incoming_call", "there is no call to answer")

			return
		}

		s.logger.ErrorContext(r.Context(), "call answer failed", "error", err)
		writeCommandError(w, err)

		return
	}

	s.logger.InfoContext(r.Context(), "call answered")
	s.BroadcastState()
	writeOK(w, http.StatusOK, map[string]any{"state": s.currentPayload()})
}

// hangupCall is idempotent: signaling.Manager.Hangup returns nil when there is
// nothing to hang up, because the media layer calls it a second time on every
// normal call teardown. There is deliberately no "nothing to hang up" error
// path here — a caller pressing hang up on a call that already ended should
// still see success.
func (s *Server) hangupCall(w http.ResponseWriter, r *http.Request) {
	if s.call == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "call control is unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), callControlTimeout)
	defer cancel()

	if err := s.call.Hangup(ctx); err != nil {
		s.logger.ErrorContext(r.Context(), "call hangup failed", "error", err)
		writeCommandError(w, err)

		return
	}

	s.logger.InfoContext(r.Context(), "call hung up")
	s.BroadcastState()
	writeOK(w, http.StatusOK, map[string]any{"state": s.currentPayload()})
}

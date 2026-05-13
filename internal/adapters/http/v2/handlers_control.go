package v2

import (
	"context"
	"errors"
	"net/http"
	"time"

	"bticino-go-companion/internal/services/control"
)

func (r *Router) handleEntrypointUnlock(w http.ResponseWriter, req *http.Request) {
	if r.control == nil {
		writeError(w, http.StatusServiceUnavailable, "control_unavailable", "control service unavailable")
		return
	}
	entrypointID := req.PathValue("id")
	ctx, cancel := contextWithTimeout(req)
	defer cancel()
	if err := r.control.UnlockEntrypoint(ctx, entrypointID); err != nil {
		handleControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "entrypoint_id": entrypointID, "action": "unlock"})
}

func (r *Router) handleEntrypointStreamStart(w http.ResponseWriter, req *http.Request) {
	if r.control == nil {
		writeError(w, http.StatusServiceUnavailable, "control_unavailable", "control service unavailable")
		return
	}
	entrypointID := req.PathValue("id")
	ctx, cancel := contextWithTimeout(req)
	defer cancel()
	if err := r.control.StartEntrypointStream(ctx, entrypointID); err != nil {
		handleControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "entrypoint_id": entrypointID, "action": "stream_start"})
}

func (r *Router) handleEntrypointStreamStop(w http.ResponseWriter, req *http.Request) {
	if r.control == nil {
		writeError(w, http.StatusServiceUnavailable, "control_unavailable", "control service unavailable")
		return
	}
	entrypointID := req.PathValue("id")
	ctx, cancel := contextWithTimeout(req)
	defer cancel()
	if err := r.control.StopEntrypointStream(ctx, entrypointID); err != nil {
		handleControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "entrypoint_id": entrypointID, "action": "stream_stop"})
}

func (r *Router) handleCallAnswer(w http.ResponseWriter, req *http.Request) {
	if r.control == nil {
		writeError(w, http.StatusServiceUnavailable, "control_unavailable", "control service unavailable")
		return
	}
	ctx, cancel := contextWithTimeout(req)
	defer cancel()
	if err := r.control.AnswerCall(ctx); err != nil {
		handleControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "call_answer"})
}

func (r *Router) handleCallHangup(w http.ResponseWriter, req *http.Request) {
	if r.control == nil {
		writeError(w, http.StatusServiceUnavailable, "control_unavailable", "control service unavailable")
		return
	}
	ctx, cancel := contextWithTimeout(req)
	defer cancel()
	if err := r.control.HangupCall(ctx); err != nil {
		handleControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "call_hangup"})
}

func (r *Router) handleAudioMute(w http.ResponseWriter, req *http.Request) {
	if r.control == nil {
		writeError(w, http.StatusServiceUnavailable, "control_unavailable", "control service unavailable")
		return
	}
	ctx, cancel := contextWithTimeout(req)
	defer cancel()
	if err := r.control.MuteAudio(ctx); err != nil {
		handleControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "audio_mute", "muted": true})
}

func (r *Router) handleAudioUnmute(w http.ResponseWriter, req *http.Request) {
	if r.control == nil {
		writeError(w, http.StatusServiceUnavailable, "control_unavailable", "control service unavailable")
		return
	}
	ctx, cancel := contextWithTimeout(req)
	defer cancel()
	if err := r.control.UnmuteAudio(ctx); err != nil {
		handleControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "audio_unmute", "muted": false})
}

func (r *Router) handleVoicemailEnable(w http.ResponseWriter, req *http.Request) {
	if r.control == nil {
		writeError(w, http.StatusServiceUnavailable, "control_unavailable", "control service unavailable")
		return
	}
	ctx, cancel := contextWithTimeout(req)
	defer cancel()
	if err := r.control.EnableVoicemail(ctx); err != nil {
		handleControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "voicemail_enable", "enabled": true})
}

func (r *Router) handleVoicemailDisable(w http.ResponseWriter, req *http.Request) {
	if r.control == nil {
		writeError(w, http.StatusServiceUnavailable, "control_unavailable", "control service unavailable")
		return
	}
	ctx, cancel := contextWithTimeout(req)
	defer cancel()
	if err := r.control.DisableVoicemail(ctx); err != nil {
		handleControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "voicemail_disable", "enabled": false})
}

func contextWithTimeout(req *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(req.Context(), 8*time.Second)
}

func handleControlError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, control.ErrEntrypointNotFound):
		writeError(w, http.StatusNotFound, "entrypoint_not_found", err.Error())
	case errors.Is(err, control.ErrCapabilityNotEnabled):
		writeError(w, http.StatusConflict, "capability_disabled", err.Error())
	case errors.Is(err, control.ErrNoIncomingCall):
		writeError(w, http.StatusConflict, "no_incoming_call", err.Error())
	case errors.Is(err, control.ErrNoActiveCall):
		writeError(w, http.StatusConflict, "no_active_call", err.Error())
	case errors.Is(err, control.ErrAudioControlDisabled):
		writeError(w, http.StatusConflict, "audio_control_disabled", err.Error())
	case errors.Is(err, control.ErrVoicemailUnavailable):
		writeError(w, http.StatusConflict, "voicemail_control_unavailable", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "control_failed", err.Error())
	}
}

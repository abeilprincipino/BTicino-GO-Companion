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

func contextWithTimeout(req *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(req.Context(), 8*time.Second)
}

func handleControlError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, control.ErrEntrypointNotFound):
		writeError(w, http.StatusNotFound, "entrypoint_not_found", err.Error())
	case errors.Is(err, control.ErrCapabilityNotEnabled):
		writeError(w, http.StatusConflict, "capability_disabled", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "control_failed", err.Error())
	}
}

package v2

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"bticino-go-companion/internal/observability"
	"bticino-go-companion/internal/services/control"
	"bticino-go-companion/internal/services/state"
)

type Router struct {
	state   *state.Projector
	control *control.Service
}

func NewRouter(projector *state.Projector, controlService *control.Service) *Router {
	return &Router{
		state:   projector,
		control: controlService,
	}
}

func (r *Router) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/health", r.handleHealth)
	mux.HandleFunc("/api/v2/state", r.handleState)
	mux.HandleFunc("/api/v2/entrypoints", r.handleEntrypoints)
	mux.HandleFunc("POST /api/v2/control/entrypoints/{id}/unlock", r.handleEntrypointUnlock)
	mux.HandleFunc("POST /api/v2/control/entrypoints/{id}/stream/start", r.handleEntrypointStreamStart)
	mux.HandleFunc("POST /api/v2/control/entrypoints/{id}/stream/stop", r.handleEntrypointStreamStop)
	return mux
}

func (r *Router) handleHealth(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	snap := r.state.Snapshot()
	writeJSON(w, http.StatusOK, observability.New(snap.BootTime, true))
}

func (r *Router) handleState(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, r.state.Snapshot())
}

func (r *Router) handleEntrypoints(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	snap := r.state.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"entrypoints": snap.Entrypoints})
}

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

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code string, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

package v2

import (
	"encoding/json"
	"net/http"

	"bticino-go-companion/internal/observability"
	"bticino-go-companion/internal/services/state"
)

type Router struct {
	state *state.Projector
}

func NewRouter(projector *state.Projector) *Router {
	return &Router{state: projector}
}

func (r *Router) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/health", r.handleHealth)
	mux.HandleFunc("/api/v2/state", r.handleState)
	mux.HandleFunc("/api/v2/entrypoints", r.handleEntrypoints)
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

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

package v2

import (
	"net/http"

	"bticino-go-companion/internal/observability"
	"bticino-go-companion/internal/services/runtime"
)

func (r *Router) handleHealth(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	snap := r.state.Snapshot()
	runtimeSnap := runtime.Snapshot{}
	if r.runtime != nil {
		runtimeSnap = r.runtime.Snapshot()
	}
	writeJSON(w, http.StatusOK, observability.New(snap.BootTime, runtimeSnap))
}

func (r *Router) handleState(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, r.state.Snapshot())
}

func (r *Router) handleCapabilities(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_version": "v2",
		"capabilities": []string{
			"entrypoints_v2",
			"events_v2",
			"control_entrypoints_v2",
		},
	})
}

func (r *Router) handleEntrypoints(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	snap := r.state.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"entrypoints": snap.Entrypoints})
}

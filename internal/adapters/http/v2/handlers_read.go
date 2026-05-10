package v2

import (
	"net/http"

	"bticino-go-companion/internal/observability"
	"bticino-go-companion/internal/services/runtime"
)

func (r *Router) handleHealth(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
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
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, r.state.Snapshot())
}

func (r *Router) handleCapabilities(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_version": "v2",
		"capabilities": []string{
			"entrypoints_v2",
			"events_v2",
			"control_entrypoints_v2",
			"control_call_v2",
			"trace_openwebnet_v2",
		},
	})
}

func (r *Router) handleEntrypoints(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	snap := r.state.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"entrypoints": snap.Entrypoints})
}

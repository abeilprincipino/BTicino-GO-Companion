package v2

import "net/http"

func (r *Router) handleOpenWebNetTrace(w http.ResponseWriter, req *http.Request) {
	if r.trace == nil {
		writeError(w, http.StatusServiceUnavailable, "trace_unavailable", "trace stream unavailable")
		return
	}
	lastID := parseLastEventID(req)
	writeJSON(w, http.StatusOK, map[string]any{
		"records": r.trace.ReplaySince(lastID),
	})
}

func (r *Router) handleOpenWebNetTraceStream(w http.ResponseWriter, req *http.Request) {
	if r.trace == nil {
		writeError(w, http.StatusServiceUnavailable, "trace_unavailable", "trace stream unavailable")
		return
	}

	lastID := parseLastEventID(req)
	replay := r.trace.ReplaySince(lastID)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "sse_unsupported", "response writer does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	for _, rec := range replay {
		if err := writeSSEEvent(w, rec); err != nil {
			return
		}
	}
	flusher.Flush()

	sub := r.trace.Subscribe(req.Context())
	for {
		select {
		case <-req.Context().Done():
			return
		case rec, ok := <-sub:
			if !ok {
				return
			}
			if err := writeSSEEvent(w, rec); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

package v2

import "net/http"

func (r *Router) handleEventsSSE(w http.ResponseWriter, req *http.Request) {
	if r.events == nil {
		writeError(w, http.StatusServiceUnavailable, "events_unavailable", "event stream unavailable")
		return
	}

	lastID := parseLastEventID(req)
	replay := r.events.ReplaySince(lastID)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "sse_unsupported", "response writer does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	for _, ev := range replay {
		if err := writeSSEEvent(w, ev); err != nil {
			return
		}
	}
	flusher.Flush()

	sub := r.events.Subscribe(req.Context())
	for {
		select {
		case <-req.Context().Done():
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			if err := writeSSEEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

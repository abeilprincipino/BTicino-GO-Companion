package v2

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code string, message string) {
	writeErrorWithExtras(w, status, code, message, nil)
}

func writeErrorWithExtras(w http.ResponseWriter, status int, code string, message string, extras map[string]any) {
	errBody := map[string]any{
		"code":      code,
		"message":   message,
		"status":    status,
		"retryable": status >= 500,
	}
	for key, value := range extras {
		errBody[key] = value
	}
	writeJSON(w, status, map[string]any{
		"error": errBody,
	})
}

func parseLastEventID(req *http.Request) uint64 {
	raw := req.URL.Query().Get("last_event_id")
	if raw == "" {
		raw = req.Header.Get("Last-Event-ID")
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func writeSSEEvent(w http.ResponseWriter, ev any) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", body)
	return err
}

func requireMethod(w http.ResponseWriter, req *http.Request, method string) bool {
	if req.Method == method {
		return true
	}
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	return false
}

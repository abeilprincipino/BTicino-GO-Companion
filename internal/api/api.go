package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
)

const maxJSONBody = 64 << 10

type StateProvider interface{ Snapshot() core.State }
type CommandHandler interface {
	HandleCommand(*http.Request, Command) (any, error)
}

type Server struct {
	auth     *auth.Store
	config   *config.Store
	state    StateProvider
	commands CommandHandler
	clients  clientSet
}

func NewServer(authStore *auth.Store, configStore *config.Store, state StateProvider, commands CommandHandler) *Server {
	return &Server{auth: authStore, config: configStore, state: state, commands: commands}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.handle(mux, "GET", "/api/v3/health", s.health)
	s.handle(mux, "GET", "/api/v3/auth/status", s.authStatus)
	s.handle(mux, "POST", "/api/v3/pair/challenge", s.pairChallenge)
	s.handle(mux, "POST", "/api/v3/pair/claim", s.pairClaim)
	s.handleProtected(mux, "POST", "/api/v3/auth/rotate", s.rotateBearer)
	s.handleProtected(mux, "POST", "/api/v3/auth/revoke", s.revokeBearer)
	s.handleProtected(mux, "POST", "/api/v3/admin/issue-repair-code", s.issueRepairCode)
	s.handleProtected(mux, "POST", "/api/v3/admin/reset-claim", s.resetClaim)
	s.handleProtected(mux, "GET", "/api/v3/state", s.stateSnapshot)
	s.handleProtected(mux, "GET", "/api/v3/entrypoints", s.entrypoints)
	s.handleProtected(mux, "GET", "/api/v3/capabilities", s.entrypoints)
	for _, path := range []string{"/api/v3/control/call/answer", "/api/v3/control/call/decline", "/api/v3/control/call/hangup", "/api/v3/control/entrypoints/{id}/unlock", "/api/v3/control/entrypoints/{id}/stream", "/api/v3/control/entrypoints/{id}/snapshot", "/api/v3/control/audio/mute", "/api/v3/control/audio/unmute", "/api/v3/control/voicemail/enable", "/api/v3/control/voicemail/disable", "/api/v3/webrtc/offer", "/api/v3/webrtc/candidate", "/api/v3/webrtc/close"} {
		s.handleProtected(mux, "POST", path, s.notImplemented)
	}
	s.handleProtected(mux, "GET", "/api/v3/entrypoints/{id}/snapshot/latest.jpg", s.notImplemented)
	mux.HandleFunc("GET /api/v3/ws", s.websocket)
	mux.HandleFunc("/api/v3/", s.notFound)
	return mux
}

func (s *Server) handle(mux *http.ServeMux, method, path string, handler http.HandlerFunc) {
	mux.HandleFunc(method+" "+path, handler)
	mux.HandleFunc(path, s.methodNotAllowed)
}
func (s *Server) handleProtected(mux *http.ServeMux, method, path string, handler http.HandlerFunc) {
	s.handle(mux, method, path, s.requireBearer(handler))
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, map[string]any{"status": "ok"})
}
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, map[string]any{"claimed": s.config != nil && s.config.Snapshot().Auth.BearerToken != ""})
}
func (s *Server) pairChallenge(w http.ResponseWriter, r *http.Request) {
	challenge, err := s.auth.CreateChallenge(sourceIP(r))
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeOK(w, http.StatusCreated, map[string]any{"challenge_id": challenge.ID, "expires_at": challenge.ExpiresAt.Format(time.RFC3339)})
}
func (s *Server) pairClaim(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChallengeID string `json:"challenge_id"`
		ClaimCode   string `json:"claim_code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	token, err := s.auth.Claim(sourceIP(r), body.ChallengeID, body.ClaimCode)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeOK(w, http.StatusOK, map[string]any{"access_token": token})
}
func (s *Server) rotateBearer(w http.ResponseWriter, r *http.Request) {
	token, err := s.auth.RotateBearer()
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeOK(w, http.StatusOK, map[string]any{"access_token": token})
}
func (s *Server) revokeBearer(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.RevokeBearer(); err != nil {
		writeAuthError(w, err)
		return
	}
	writeOK(w, http.StatusOK, nil)
}
func (s *Server) issueRepairCode(w http.ResponseWriter, r *http.Request) {
	code, err := s.auth.IssueRepairCode()
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeOK(w, http.StatusOK, map[string]any{"repair_code": code})
}
func (s *Server) resetClaim(w http.ResponseWriter, r *http.Request) {
	code, err := s.auth.ResetRepairCode()
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeOK(w, http.StatusOK, map[string]any{"repair_code": code})
}
func (s *Server) stateSnapshot(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, map[string]any{"state": s.snapshot()})
}
func (s *Server) entrypoints(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "configuration is unavailable")
		return
	}
	writeOK(w, http.StatusOK, map[string]any{"entrypoints": s.config.Snapshot().Companion.Entrypoints})
}
func (s *Server) notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented", "this endpoint is not implemented")
}
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
}
func (s *Server) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed")
}
func (s *Server) requireBearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" || s.auth == nil || !s.auth.ValidateBearer(token) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
			return
		}
		next(w, r)
	}
}
func (s *Server) snapshot() core.State {
	if s.state == nil {
		return core.State{CallState: core.CallStateIdle}
	}
	return s.state.Snapshot()
}
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
		return false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return false
	}
	return true
}
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidSourceIP):
		writeError(w, http.StatusBadRequest, "invalid_source_ip", "source address is invalid")
	case errors.Is(err, auth.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "rate_limited", "claim attempts are rate limited")
	case errors.Is(err, auth.ErrChallengeNotFound), errors.Is(err, auth.ErrChallengeExpired), errors.Is(err, auth.ErrChallengeSourceMismatch), errors.Is(err, auth.ErrInvalidClaimCode):
		writeError(w, http.StatusUnauthorized, "claim_failed", "claim could not be completed")
	case errors.Is(err, auth.ErrStoreUnavailable):
		writeError(w, http.StatusServiceUnavailable, "unavailable", "authentication is unavailable")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "request could not be completed")
	}
}
func writeOK(w http.ResponseWriter, status int, payload map[string]any) {
	response := map[string]any{"ok": true}
	for key, value := range payload {
		response[key] = value
	}
	writeJSON(w, status, response)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": map[string]string{"code": code, "message": message}})
}
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

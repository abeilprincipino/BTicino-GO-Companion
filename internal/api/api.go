package api

import (
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/httputil"
	"bticino-go-companion/internal/logging"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"strings"
	"time"
)

const maxJSONBody = 64 << 10

type (
	StateProvider  interface{ Snapshot() core.State }
	CommandHandler interface {
		HandleCommand(*http.Request, Command) (any, error)
	}
)

type Server struct {
	auth     *auth.Store
	config   *config.Store
	state    StateProvider
	commands CommandHandler
	clients  clientSet
	webrtc   WebRTCControl
	snapshot SnapshotControl
	runtime  RuntimeControl
	update   UpdateControl
	logger   *slog.Logger
}

func NewServer(authStore *auth.Store, configStore *config.Store, state StateProvider, commands CommandHandler, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	return &Server{auth: authStore, config: configStore, state: state, commands: commands, logger: logger}
}

func (s *Server) SetWebRTC(v WebRTCControl)     { s.webrtc = v }
func (s *Server) SetSnapshot(v SnapshotControl) { s.snapshot = v }
func (s *Server) SetRuntime(v RuntimeControl)   { s.runtime = v }
func (s *Server) SetUpdate(v UpdateControl)     { s.update = v }

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
	s.handleProtected(mux, "GET", "/api/v3/entrypoints", s.handleEntrypoints)
	s.handleProtected(mux, "GET", "/api/v3/capabilities", s.handleCapabilities)

	// Call control is not exposed until it controls the physical intercom.
	s.handleProtected(mux, "POST", "/api/v3/control/entrypoints/{id}/unlock", s.entrypointCommand("unlock"))
	s.handleProtected(mux, "POST", "/api/v3/control/audio/mute", s.command("audio.mute"))
	s.handleProtected(mux, "POST", "/api/v3/control/audio/unmute", s.command("audio.unmute"))
	s.handleProtected(mux, "POST", "/api/v3/control/voicemail/enable", s.command("voicemail.enable"))
	s.handleProtected(mux, "POST", "/api/v3/control/voicemail/disable", s.command("voicemail.disable"))
	s.handleProtected(mux, "GET", "/api/v3/ws", s.websocket)
	mux.HandleFunc("/api/v3/", s.notFound)

	return logging.HTTP(s.logger, mux)
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
	writeOK(w, http.StatusOK, map[string]any{"state": s.currentState()})
}

func (s *Server) handleEntrypoints(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "configuration is unavailable")
		return
	}

	writeOK(w, http.StatusOK, map[string]any{"entrypoints": s.config.Snapshot().Companion.Entrypoints})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "configuration is unavailable")
		return
	}

	caps := make([]config.Capabilities, 0, len(s.config.Snapshot().Companion.Entrypoints))
	for _, ep := range s.config.Snapshot().Companion.Entrypoints {
		caps = append(caps, ep.Capabilities)
	}

	writeOK(w, http.StatusOK, map[string]any{"capabilities": caps})
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

func (s *Server) currentState() core.State {
	if s.state == nil {
		return core.State{CallState: core.CallStateIdle}
	}

	return s.state.Snapshot()
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := httputil.DecodeJSON(r.Body, maxJSONBody, target); err != nil {
		if errors.Is(err, httputil.ErrMultipleJSONValues) {
			writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be valid JSON")
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
	maps.Copy(response, payload)

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

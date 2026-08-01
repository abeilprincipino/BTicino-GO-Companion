package api

import (
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/diagnostics"
	"bticino-go-companion/internal/httputil"
	"bticino-go-companion/internal/logging"
	"context"
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
	StateProvider interface{ Snapshot() core.State }
)

type Server struct {
	auth             *auth.Store
	config           *config.Store
	state            StateProvider
	entrypoints      EntrypointControl
	audio            AudioControl
	voicemail        VoicemailControl
	refreshVoicemail func(context.Context) (bool, error)
	clients          clientSet
	webrtcClients    clientSet
	webrtc           WebRTCControl
	snapshot         SnapshotControl
	runtime          RuntimeControl
	update           UpdateControl
	call             CallControl
	diagnostics      interface {
		Snapshot() diagnostics.Snapshot
		Refresh(context.Context)
	}
	logger *slog.Logger
}

func NewServer(authStore *auth.Store, configStore *config.Store, state StateProvider, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	return &Server{auth: authStore, config: configStore, state: state, logger: logger.With("component", "api")}
}

func (s *Server) SetEntrypoints(v EntrypointControl) { s.entrypoints = v }
func (s *Server) SetAudio(v AudioControl)            { s.audio = v }
func (s *Server) SetVoicemail(v VoicemailControl)    { s.voicemail = v }
func (s *Server) SetVoicemailRefresh(refresh func(context.Context) (bool, error)) {
	s.refreshVoicemail = refresh
}
func (s *Server) SetWebRTC(v WebRTCControl)     { s.webrtc = v }
func (s *Server) SetSnapshot(v SnapshotControl) { s.snapshot = v }
func (s *Server) SetRuntime(v RuntimeControl)   { s.runtime = v }
func (s *Server) SetUpdate(v UpdateControl)     { s.update = v }
func (s *Server) SetCall(v CallControl)         { s.call = v }
func (s *Server) SetDiagnostics(v interface {
	Snapshot() diagnostics.Snapshot
	Refresh(context.Context)
},
) {
	s.diagnostics = v
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.handle(mux, "GET", "/api/v3/health", s.health)
	s.handle(mux, "GET", "/api/v3/auth/status", s.authStatus)
	s.handle(mux, "POST", "/api/v3/pair/challenge", s.pairChallenge)
	s.handle(mux, "POST", "/api/v3/pair/claim", s.pairClaim)
	s.handleProtected(mux, "POST", "/api/v3/auth/rotate", s.rotateBearer)
	s.handleProtected(mux, "POST", "/api/v3/auth/revoke", s.revokeBearer)
	s.handle(mux, "POST", "/api/v3/auth/recover", s.recoverBearer)
	s.handleProtected(mux, "GET", "/api/v3/state", s.stateSnapshot)
	s.handleProtected(mux, "GET", "/api/v3/diagnostics", s.diagnosticsSnapshot)
	mux.HandleFunc("POST /api/v3/diagnostics", s.requireBearer(s.diagnosticsRefresh))
	s.handleProtected(mux, "GET", "/api/v3/system/update/status", s.systemUpdateStatus)
	s.handleProtected(mux, "POST", "/api/v3/system/update/check", s.systemUpdateCheck)
	s.handleProtected(mux, "POST", "/api/v3/system/update/stage", s.systemUpdateStage)
	s.handleProtected(mux, "POST", "/api/v3/system/update/install", s.systemUpdateInstall)
	s.handleProtected(mux, "POST", "/api/v3/system/reboot", s.systemReboot)
	s.handleProtected(mux, "POST", "/api/v3/system/services/{name}/restart", s.systemServiceRestart)

	s.handleProtected(mux, "POST", "/api/v3/entrypoints/{id}/unlock", s.unlockEntrypoint)
	s.handleProtected(mux, "POST", "/api/v3/audio/mute", s.muteAudio)
	s.handleProtected(mux, "POST", "/api/v3/audio/unmute", s.unmuteAudio)
	s.handleProtected(mux, "POST", "/api/v3/call/answer", s.answerCall)
	s.handleProtected(mux, "POST", "/api/v3/call/hangup", s.hangupCall)
	s.handleProtected(mux, "POST", "/api/v3/voicemail/enable", s.enableVoicemail)
	s.handleProtected(mux, "POST", "/api/v3/voicemail/disable", s.disableVoicemail)
	s.handleProtected(mux, "POST", "/api/v3/voicemail/refresh", s.voicemailRefresh)
	s.handleProtected(mux, "GET", "/api/v3/ws", s.websocket)
	s.handleProtected(mux, "GET", "/api/v3/webrtc/ws", s.webrtcWebsocket)
	s.handleProtected(mux, "GET", "/api/v3/entrypoints/{id}/snapshot/latest.jpg", s.snapshotLatest)
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
	if s.auth == nil || s.config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "authentication is unavailable")
		return
	}

	status := s.auth.Status()
	cfg := s.config.Snapshot()
	writeOK(w, http.StatusOK, map[string]any{
		"device_id":     cfg.Companion.DeviceID,
		"instance_id":   status.InstanceID,
		"model":         cfg.Companion.Model,
		"pairing_state": status.State,
	})
}

func (s *Server) pairChallenge(w http.ResponseWriter, r *http.Request) {
	challenge, err := s.auth.CreateChallenge(sourceIP(r))
	if err != nil {
		s.logger.DebugContext(r.Context(), "pairing challenge rejected", "client_ip", sourceIP(r), "error", err)
		writeAuthError(w, err)

		return
	}

	s.logger.InfoContext(r.Context(), "pairing challenge created", "client_ip", sourceIP(r))

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
		s.logger.DebugContext(r.Context(), "pairing claim rejected", "client_ip", sourceIP(r), "error", err)
		writeAuthError(w, err)

		return
	}

	s.logger.InfoContext(r.Context(), "pairing completed", "client_ip", sourceIP(r))

	writeOK(w, http.StatusOK, map[string]any{"access_token": token})
}

func (s *Server) rotateBearer(w http.ResponseWriter, r *http.Request) {
	token, err := s.auth.RotateBearer()
	if err != nil {
		s.logger.DebugContext(r.Context(), "bearer rotation rejected", "client_ip", sourceIP(r), "error", err)
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

func (s *Server) recoverBearer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RepairCode string `json:"repair_code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	token, err := s.auth.RecoverBearer(body.RepairCode)
	if err != nil {
		writeAuthError(w, err)
		return
	}

	s.logger.InfoContext(r.Context(), "bearer recovered", "client_ip", sourceIP(r))
	writeOK(w, http.StatusOK, map[string]any{"access_token": token})
}

func (s *Server) stateSnapshot(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, map[string]any{"state": s.currentPayload()})
}

func (s *Server) diagnosticsSnapshot(w http.ResponseWriter, r *http.Request) {
	writeOK(w, http.StatusOK, map[string]any{"diagnostics": s.currentDiagnostics()})
}

func (s *Server) diagnosticsRefresh(w http.ResponseWriter, r *http.Request) {
	if s.diagnostics == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "diagnostics are unavailable")
		return
	}
	go s.diagnostics.Refresh(context.WithoutCancel(r.Context()))

	writeOK(w, http.StatusAccepted, map[string]any{"diagnostics": s.currentDiagnostics()})
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
			s.logger.DebugContext(r.Context(), "bearer authentication rejected", "route", r.Pattern, "client_ip", sourceIP(r))
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

func (s *Server) currentDiagnostics() diagnostics.Snapshot {
	if s.diagnostics == nil {
		return diagnostics.Snapshot{}
	}

	return s.diagnostics.Snapshot()
}

func (s *Server) currentPayload() StateDTO {
	diagnostic := s.currentDiagnostics()

	dto := StateDTO{State: s.currentState(), Diagnostics: diagnostic}
	if s.config != nil {
		cfg := s.config.Snapshot()
		dto.Device.Model = cfg.Companion.Model

		dto.Entrypoints = make([]EntrypointDTO, 0, len(cfg.Companion.Entrypoints))
		for _, entrypoint := range cfg.Companion.Entrypoints {
			dto.Entrypoints = append(dto.Entrypoints, entrypointDTO(entrypoint, s.entrypoints != nil))
		}

		dto.SystemControl.RebootEnabled = cfg.System.RebootEnabled && s.runtime != nil && s.runtime.RebootAvailable()

		dto.SystemControl.Services = make(map[string]SystemServiceDTO, len(cfg.System.Services))
		for name, service := range cfg.System.Services {
			dto.SystemControl.Services[name] = SystemServiceDTO{Enabled: service.Enabled, Exposed: service.Exposed && s.runtime != nil && s.runtime.ServiceAvailable(name)}
		}
	}

	dto.Device.Firmware = diagnostic.OpenWebNet.Firmware
	dto.Device.Hardware = diagnostic.OpenWebNet.Hardware

	if s.update != nil {
		if update, err := s.update.Status(context.Background()); err == nil {
			dto.SystemControl.Update = &update
		}
	}

	return dto
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
	case errors.Is(err, auth.ErrInvalidRepairCode), errors.Is(err, auth.ErrRepairCodeExpired):
		writeError(w, http.StatusUnauthorized, "repair_code_invalid", "repair code is invalid or expired")
	case errors.Is(err, auth.ErrRepairNotAllowed):
		writeError(w, http.StatusConflict, "repair_not_allowed", "claim reset is not currently allowed")
	case errors.Is(err, auth.ErrSetupRequired):
		writeError(w, http.StatusConflict, "setup_required", "companion owner setup is required")
	case errors.Is(err, auth.ErrClaimNotAllowed):
		writeError(w, http.StatusConflict, "claim_not_allowed", "initial claim is not currently allowed")
	case errors.Is(err, auth.ErrAlreadyClaimed):
		writeError(w, http.StatusConflict, "already_claimed", "device is already claimed")
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

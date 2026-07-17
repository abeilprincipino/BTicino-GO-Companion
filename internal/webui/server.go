package webui

import (
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/diagnostics"
	"bticino-go-companion/internal/httputil"
	"bticino-go-companion/internal/logging"
	"bticino-go-companion/internal/system"
	"bticino-go-companion/web"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	defaultUsername = "companion"
	defaultPassword = "companion"
	sessionCookie   = "companion_web_session"
	sessionIdle     = 30 * time.Minute
	sessionLifetime = 12 * time.Hour
	maxSessions     = 32
	restartTimeout  = 30 * time.Second
)

type RestartFunc func(context.Context) error

type FrameProvider interface {
	Frames() []map[string]any
}

type DiagnosticsProvider interface{ Snapshot() diagnostics.Snapshot }
type UpdateProvider interface {
	Status(context.Context) (system.UpdateStatus, error)
	Install(context.Context) (system.UpdateStatus, error)
}

type Server struct {
	config      *config.Store
	auth        *auth.Store
	logger      *slog.Logger
	restart     RestartFunc
	setLogLevel func(string) error
	frames      FrameProvider
	diagnostics DiagnosticsProvider
	update      UpdateProvider

	mu       sync.Mutex
	sessions map[string]session

	restartMu      sync.Mutex
	restartPending bool
	bootTime       time.Time
}

type session struct {
	bootstrap bool
	secret    string
	createdAt time.Time
	lastSeen  time.Time
}

type editableConfig struct {
	Companion editableCompanion `json:"companion"`
	System    editableSystem    `json:"system"`
	HomeKit   editableHomeKit   `json:"homekit"`
}

type editableCompanion struct {
	Name        string              `json:"name"`
	LogLevel    string              `json:"log_level"`
	Entrypoints []config.Entrypoint `json:"entrypoints"`
}

type editableSystem struct {
	RebootEnabled bool                      `json:"reboot_enabled"`
	UpdateEnabled bool                      `json:"update_enabled"`
	UpdateExposed bool                      `json:"update_exposed"`
	Services      map[string]config.Service `json:"services"`
}

type editableHomeKit struct {
	Enabled bool `json:"enabled"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type passwordRequest struct {
	Username        string `json:"username"`
	CurrentPassword string `json:"current_password"`
	Password        string `json:"password"`
}

func New(store *config.Store, authStore *auth.Store, logger *slog.Logger, restart RestartFunc, setLogLevel func(string) error) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	return &Server{config: store, auth: authStore, logger: logger, restart: restart, setLogLevel: setLogLevel, sessions: make(map[string]session), bootTime: time.Now()}
}

func (s *Server) SetFrames(provider FrameProvider)            { s.frames = provider }
func (s *Server) SetDiagnostics(provider DiagnosticsProvider) { s.diagnostics = provider }
func (s *Server) SetUpdate(provider UpdateProvider)           { s.update = provider }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /webui/api/session", s.handleSession)
	mux.HandleFunc("POST /webui/api/login", s.handleLogin)
	mux.HandleFunc("POST /webui/api/logout", s.handleLogout)
	mux.HandleFunc("POST /webui/api/password", s.handlePassword)
	mux.HandleFunc("POST /webui/api/credentials", s.handlePassword)
	mux.HandleFunc("GET /webui/api/pairing", s.requireReady(s.handlePairing))
	mux.HandleFunc("POST /webui/api/repair-code", s.requireReady(s.handleRepairCode))
	mux.HandleFunc("GET /webui/api/config", s.requireReady(s.handleConfig))
	mux.HandleFunc("PUT /webui/api/config", s.requireReady(s.handleConfig))
	mux.HandleFunc("POST /webui/api/restart", s.requireReady(s.handleRestart))
	mux.HandleFunc("GET /webui/api/status", s.requireReady(s.handleStatus))
	mux.HandleFunc("GET /webui/api/update/status", s.requireReady(s.handleUpdateStatus))
	mux.HandleFunc("POST /webui/api/update/install", s.requireReady(s.handleUpdateInstall))
	mux.HandleFunc("GET /webui/api/logs", s.requireReady(s.handleLogs))
	mux.HandleFunc("GET /webui/api/logging", s.requireReady(s.handleLogging))
	mux.HandleFunc("PUT /webui/api/logging", s.requireReady(s.handleLogging))
	mux.HandleFunc("GET /webui/api/frames", s.requireReady(s.handleFrames))
	mux.Handle("/", s.staticHandler())

	return logging.HTTP(s.logger, mux)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg, ok := s.snapshot()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "configuration is unavailable")
		return
	}

	diagnostic := diagnostics.Snapshot{}
	if s.diagnostics != nil {
		diagnostic = s.diagnostics.Snapshot()
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": cfg.Companion.Model, "device_id": cfg.Companion.DeviceID, "firmware": diagnostic.OpenWebNet.Firmware, "hardware": diagnostic.OpenWebNet.Hardware, "uptime_seconds": int64(time.Since(s.bootTime).Seconds()), "free_ram_kb": readMemAvailableKB(), "wifi_strength": diagnostic.Local.WiFiStrength, "diagnostics": diagnostic})
}

func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if s.update == nil {
		writeError(w, http.StatusServiceUnavailable, "update status is unavailable")
		return
	}
	status, err := s.update.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "update status is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	if s.update == nil {
		writeError(w, http.StatusServiceUnavailable, "update control is unavailable")
		return
	}

	status, err := s.update.Install(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "update install failed")
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

func readMemAvailableKB() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemAvailable:" {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err == nil {
			return value
		}
	}
	return 0
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(logging.Path)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "read log failed")
		return
	}
	if len(data) > 512<<10 {
		data = data[len(data)-(512<<10):]
	}

	writeJSON(w, http.StatusOK, map[string]any{"log": string(data)})
}

func (s *Server) handleLogging(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg, ok := s.snapshot()
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "configuration is unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"level": cfg.Companion.LogLevel})
		return
	}

	var body struct {
		Level string `json:"level"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if s.setLogLevel == nil || s.setLogLevel(body.Level) != nil {
		writeError(w, http.StatusBadRequest, "invalid log level")
		return
	}
	if err := s.config.Update(func(cfg *config.Config) error { cfg.Companion.LogLevel = body.Level; return nil }); err != nil {
		writeError(w, http.StatusInternalServerError, "save log level failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"level": body.Level})
}

func (s *Server) handleFrames(w http.ResponseWriter, _ *http.Request) {
	if s.frames == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "frames": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "frames": s.frames.Frames()})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	cfg, ok := s.snapshot()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "configuration is unavailable")
		return
	}

	current, authenticated := s.currentSession(r, cfg)
	if authenticated {
		refreshSessionCookie(w, r)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":            authenticated,
		"password_change_required": authenticated && current.bootstrap,
		"bootstrap":                authenticated && current.bootstrap,
		"bootstrap_required":       cfg.WebUI.AdminPasswordHash == "",
		"username":                 cfg.WebUI.AdminUsername,
		"version":                  system.BuildVersion,
		"git_sha":                  system.BuildGitSHA,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}

	var request loginRequest
	if !decodeJSON(w, r, &request) {
		return
	}

	cfg, ok := s.snapshot()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "configuration is unavailable")
		return
	}

	bootstrap := strings.TrimSpace(cfg.WebUI.AdminPasswordHash) == ""

	valid := request.Username == defaultUsername
	if bootstrap {
		valid = valid && request.Password == defaultPassword
	} else {
		valid = request.Username == cfg.WebUI.AdminUsername && bcrypt.CompareHashAndPassword([]byte(cfg.WebUI.AdminPasswordHash), []byte(request.Password)) == nil
	}

	if !valid {
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}

	token, err := config.RandomHex(32)
	if err != nil {
		s.logger.Error("create webui session", "error", err)
		writeError(w, http.StatusInternalServerError, "create session failed")

		return
	}

	s.mu.Lock()
	s.pruneSessions(time.Now())
	if len(s.sessions) >= maxSessions {
		s.removeOldestSession()
	}
	now := time.Now()
	s.sessions[token] = session{bootstrap: bootstrap, secret: cfg.WebUI.SessionSecret, createdAt: now, lastSeen: now}
	s.mu.Unlock()
	setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "password_change_required": bootstrap})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}

	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, cookie.Value)
		s.mu.Unlock()
	}

	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePairing(w http.ResponseWriter, _ *http.Request) {
	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "authentication is unavailable")
		return
	}
	cfg, ok := s.snapshot()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "configuration is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"claimed": !s.auth.NeedsClaim(), "claim_code": cfg.Auth.ClaimCode})
}

func (s *Server) handleRepairCode(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	if s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "authentication is unavailable")
		return
	}
	code, expiresAt, err := s.auth.IssueRepairCode()
	if err != nil {
		s.logger.Warn("issue repair code", "error", err)
		writeError(w, http.StatusConflict, "repair code is unavailable")
		return
	}
	s.logger.InfoContext(r.Context(), "webui repair code issued", "expires_at", expiresAt)
	writeJSON(w, http.StatusOK, map[string]any{"repair_code": code, "expires_at": expiresAt, "ttl_s": int(auth.RepairCodeLifetime.Seconds())})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}

	var request passwordRequest
	if !decodeJSON(w, r, &request) {
		return
	}

	cfg, ok := s.snapshot()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "configuration is unavailable")
		return
	}

	current, authenticated := s.currentSession(r, cfg)
	if !authenticated {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	refreshSessionCookie(w, r)

	request.Username = strings.TrimSpace(request.Username)
	if request.Username == "" || len(request.Password) < 8 || (current.bootstrap && (request.Password == defaultPassword || request.Username == defaultUsername)) {
		writeError(w, http.StatusBadRequest, "password must replace the default and contain at least 8 characters")
		return
	}

	if !current.bootstrap && bcrypt.CompareHashAndPassword([]byte(cfg.WebUI.AdminPasswordHash), []byte(request.CurrentPassword)) != nil {
		writeError(w, http.StatusUnauthorized, "current password is invalid")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(request.Password), bcrypt.DefaultCost)
	if err != nil {
		s.logger.Error("hash webui password", "error", err)
		writeError(w, http.StatusInternalServerError, "save password failed")

		return
	}

	secret, err := config.RandomHex(32)
	if err != nil {
		s.logger.Error("create webui session secret", "error", err)
		writeError(w, http.StatusInternalServerError, "save password failed")

		return
	}

	if err := s.config.Update(func(cfg *config.Config) error {
		cfg.WebUI.AdminUsername = request.Username
		cfg.WebUI.AdminPasswordHash = string(hash)
		cfg.WebUI.SessionSecret = secret

		return nil
	}); err != nil {
		s.logger.Error("save webui password", "error", err)
		writeError(w, http.StatusInternalServerError, "save password failed")

		return
	}

	s.mu.Lock()
	s.sessions = make(map[string]session)
	s.mu.Unlock()
	clearSessionCookie(w)
	response := map[string]any{"ok": true}
	if current.bootstrap && s.auth != nil {
		claimCode, err := s.auth.IssueInitialClaimCode()
		if err != nil && !errors.Is(err, auth.ErrAlreadyClaimed) {
			s.logger.Error("issue initial claim code", "error", err)
			writeError(w, http.StatusInternalServerError, "save password failed")
			return
		}
		response["claim_code"] = claimCode
		s.logger.Info("webui owner setup completed", "claim_code_issued", claimCode != "")
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, ok := s.snapshot()
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "configuration is unavailable")
			return
		}

		writeJSON(w, http.StatusOK, redactedConfig(cfg))
	case http.MethodPut:
		if !s.sameOrigin(w, r) {
			return
		}

		var next editableConfig
		if !decodeJSON(w, r, &next) {
			return
		}

		if err := s.config.Update(func(cfg *config.Config) error {
			cfg.Companion.Name = strings.TrimSpace(next.Companion.Name)
			cfg.Companion.LogLevel = strings.TrimSpace(next.Companion.LogLevel)
			cfg.Companion.Entrypoints = next.Companion.Entrypoints
			cfg.System.RebootEnabled = next.System.RebootEnabled
			cfg.System.UpdateEnabled = next.System.UpdateEnabled
			cfg.System.UpdateExposed = next.System.UpdateExposed
			cfg.System.Services = next.System.Services
			cfg.HomeKit.Enabled = next.HomeKit.Enabled

			return nil
		}); err != nil {
			s.logger.Error("save webui config", "error", err)
			writeError(w, http.StatusBadRequest, "configuration is invalid")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}

	if s.restart == nil {
		writeError(w, http.StatusServiceUnavailable, "restart is unavailable")
		return
	}

	s.restartMu.Lock()
	if s.restartPending {
		s.restartMu.Unlock()
		writeError(w, http.StatusConflict, "restart is already in progress")
		return
	}
	s.restartPending = true
	s.restartMu.Unlock()

	restartCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), restartTimeout)
	go s.runRestart(restartCtx, cancel)

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "restarting": true})
}

func (s *Server) runRestart(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	defer func() {
		s.restartMu.Lock()
		s.restartPending = false
		s.restartMu.Unlock()
	}()

	s.logger.InfoContext(ctx, "restart companion requested")
	if err := s.restart(ctx); err != nil {
		s.logger.ErrorContext(ctx, "restart companion", "error", err)
	}
}

func (s *Server) requireReady(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg, ok := s.snapshot()
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "configuration is unavailable")
			return
		}

		current, authenticated := s.currentSession(r, cfg)
		if !authenticated {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		refreshSessionCookie(w, r)

		if current.bootstrap {
			writeError(w, http.StatusForbidden, "password change required")
			return
		}

		next(w, r)
	}
}

func (s *Server) snapshot() (config.Config, bool) {
	if s.config == nil {
		return config.Config{}, false
	}

	return s.config.Snapshot(), true
}

func (s *Server) currentSession(r *http.Request, cfg config.Config) (session, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return session{}, false
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneSessions(now)
	current, ok := s.sessions[cookie.Value]

	if !ok {
		return session{}, false
	}

	valid := current.bootstrap && cfg.WebUI.AdminPasswordHash == ""
	if !current.bootstrap {
		valid = cfg.WebUI.SessionSecret != "" && subtle.ConstantTimeCompare([]byte(current.secret), []byte(cfg.WebUI.SessionSecret)) == 1
	}
	if !valid {
		delete(s.sessions, cookie.Value)
		return session{}, false
	}

	current.lastSeen = now
	s.sessions[cookie.Value] = current
	return current, true
}

func (s *Server) pruneSessions(now time.Time) {
	for token, current := range s.sessions {
		if now.Sub(current.createdAt) >= sessionLifetime || now.Sub(current.lastSeen) >= sessionIdle {
			delete(s.sessions, token)
		}
	}
}

func (s *Server) removeOldestSession() {
	var oldestToken string
	var oldest time.Time
	for token, current := range s.sessions {
		if oldestToken == "" || current.lastSeen.Before(oldest) {
			oldestToken = token
			oldest = current.lastSeen
		}
	}
	delete(s.sessions, oldestToken)
}

func (s *Server) sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}

	parsed, err := url.Parse(origin)
	if err == nil && parsed.Scheme == "http" && parsed.Host == r.Host {
		return true
	}

	writeError(w, http.StatusForbidden, "origin is not allowed")

	return false
}

func redactedConfig(cfg config.Config) editableConfig {
	return editableConfig{
		Companion: editableCompanion{Name: cfg.Companion.Name, LogLevel: cfg.Companion.LogLevel, Entrypoints: cfg.Companion.Entrypoints},
		System:    editableSystem{RebootEnabled: cfg.System.RebootEnabled, UpdateEnabled: cfg.System.UpdateEnabled, UpdateExposed: cfg.System.UpdateExposed, Services: cfg.System.Services},
		HomeKit:   editableHomeKit{Enabled: cfg.HomeKit.Enabled},
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close() //nolint:errcheck // request body already consumed

	if err := httputil.DecodeJSON(r.Body, 64<<10, dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}

	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionIdle.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func refreshSessionCookie(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookie)
	if err == nil {
		setSessionCookie(w, cookie.Value)
	}
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (s *Server) staticHandler() http.Handler {
	files, err := fs.Sub(web.Files, ".")
	if err != nil {
		return http.NotFoundHandler()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/webui/api/") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}

		if r.URL.Path == "/" || r.URL.Path == "/webui/" {
			data, err := fs.ReadFile(files, "index.html")
			if err != nil {
				writeError(w, http.StatusInternalServerError, "static files are unavailable")
				return
			}

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)

			return
		}

		http.FileServer(http.FS(files)).ServeHTTP(w, r)
	})
}

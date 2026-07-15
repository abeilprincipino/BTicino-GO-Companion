package webui

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/logging"
	"bticino-go-companion/web"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	defaultUsername = "companion"
	defaultPassword = "companion"
	sessionCookie   = "companion_web_session"
)

type RestartFunc func(context.Context) error

type Server struct {
	config  *config.Store
	logger  *slog.Logger
	restart RestartFunc

	mu       sync.Mutex
	sessions map[string]session
}

type session struct {
	bootstrap bool
	secret    string
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
	AllowRollback bool                      `json:"allow_rollback"`
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
	CurrentPassword string `json:"current_password"`
	Password        string `json:"password"`
}

func New(store *config.Store, logger *slog.Logger, restart RestartFunc) *Server {
	if logger == nil {
		logger = slog.Default()
	}

	return &Server{config: store, logger: logger, restart: restart, sessions: make(map[string]session)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /webui/api/session", s.handleSession)
	mux.HandleFunc("POST /webui/api/login", s.handleLogin)
	mux.HandleFunc("POST /webui/api/logout", s.handleLogout)
	mux.HandleFunc("POST /webui/api/password", s.handlePassword)
	mux.HandleFunc("GET /webui/api/config", s.requireReady(s.handleConfig))
	mux.HandleFunc("PUT /webui/api/config", s.requireReady(s.handleConfig))
	mux.HandleFunc("POST /webui/api/restart", s.requireReady(s.handleRestart))
	mux.Handle("/", s.staticHandler())

	return logging.HTTP(s.logger, mux)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	cfg, ok := s.snapshot()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "configuration is unavailable")
		return
	}

	current, authenticated := s.currentSession(r, cfg)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":            authenticated,
		"password_change_required": authenticated && current.bootstrap,
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
		valid = valid && bcrypt.CompareHashAndPassword([]byte(cfg.WebUI.AdminPasswordHash), []byte(request.Password)) == nil
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
	s.sessions[token] = session{bootstrap: bootstrap, secret: cfg.WebUI.SessionSecret}
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

	if len(request.Password) < 8 || (current.bootstrap && request.Password == defaultPassword) {
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
			cfg.System.AllowRollback = next.System.AllowRollback
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

	go func() {
		time.Sleep(250 * time.Millisecond)

		if err := s.restart(context.Background()); err != nil {
			s.logger.Error("restart companion", "error", err)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "restarting": true})
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

	s.mu.Lock()
	current, ok := s.sessions[cookie.Value]
	s.mu.Unlock()

	if !ok {
		return session{}, false
	}

	if current.bootstrap {
		return current, cfg.WebUI.AdminPasswordHash == ""
	}

	return current, cfg.WebUI.SessionSecret != "" && subtle.ConstantTimeCompare([]byte(current.secret), []byte(cfg.WebUI.SessionSecret)) == 1
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
		System:    editableSystem{RebootEnabled: cfg.System.RebootEnabled, UpdateEnabled: cfg.System.UpdateEnabled, UpdateExposed: cfg.System.UpdateExposed, AllowRollback: cfg.System.AllowRollback, Services: cfg.System.Services},
		HomeKit:   editableHomeKit{Enabled: cfg.HomeKit.Enabled},
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close() //nolint:errcheck // request body already consumed

	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}

	if decoder.Decode(&struct{}{}) != io.EOF {
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
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
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

package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bticino-go-companion/internal/config"
)

func TestBootstrapRequiresPasswordChange(t *testing.T) {
	server, _ := testServer(t, nil)

	response := request(t, server, http.MethodPost, "/webui/api/login", loginRequest{Username: defaultUsername, Password: defaultPassword}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap login status = %d", response.Code)
	}
	cookie := sessionCookieFrom(t, response)

	response = request(t, server, http.MethodGet, "/webui/api/config", nil, cookie)
	if response.Code != http.StatusForbidden {
		t.Fatalf("bootstrap config status = %d", response.Code)
	}

	response = request(t, server, http.MethodPost, "/webui/api/password", passwordRequest{Password: "correct-horse"}, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("password change status = %d: %s", response.Code, response.Body.String())
	}

	response = request(t, server, http.MethodGet, "/webui/api/config", nil, cookie)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("old bootstrap session status = %d", response.Code)
	}
}

func TestConfigIsRedactedAndSavedThroughStore(t *testing.T) {
	server, store := testServer(t, nil)
	cookie := configuredSession(t, server)

	response := request(t, server, http.MethodGet, "/webui/api/config", nil, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("config status = %d", response.Code)
	}
	if strings.Contains(response.Body.String(), "claim_code") || strings.Contains(response.Body.String(), "bearer_token") || strings.Contains(response.Body.String(), "session_secret") {
		t.Fatalf("redacted config contains secret: %s", response.Body.String())
	}
	var editable editableConfig
	decodeResponse(t, response, &editable)
	editable.Companion.Name = "Updated Companion"

	response = request(t, server, http.MethodPut, "/webui/api/config", editable, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("save config status = %d: %s", response.Code, response.Body.String())
	}
	if store.Snapshot().Companion.Name != "Updated Companion" {
		t.Fatalf("saved companion name = %q", store.Snapshot().Companion.Name)
	}
	if store.Snapshot().Auth.BearerToken != "bearer-secret" || store.Snapshot().Auth.ClaimCode != "01234567" {
		t.Fatal("config save changed auth secrets")
	}
}

func TestPasswordChangeRotatesSecretAndInvalidatesEverySession(t *testing.T) {
	server, store := testServer(t, nil)
	first := configuredSession(t, server)
	second := passwordSession(t, server, "correct-horse")
	previousSecret := store.Snapshot().WebUI.SessionSecret

	response := request(t, server, http.MethodPost, "/webui/api/password", passwordRequest{CurrentPassword: "correct-horse", Password: "new-password"}, first)
	if response.Code != http.StatusOK {
		t.Fatalf("password change status = %d: %s", response.Code, response.Body.String())
	}
	if store.Snapshot().WebUI.SessionSecret == previousSecret {
		t.Fatal("session secret did not rotate")
	}

	response = request(t, server, http.MethodGet, "/webui/api/config", nil, second)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("old session status = %d", response.Code)
	}
}

func TestRestartRequiresConfiguredSession(t *testing.T) {
	restarted := make(chan struct{}, 1)
	server, _ := testServer(t, func(context.Context) error {
		restarted <- struct{}{}
		return nil
	})

	response := request(t, server, http.MethodPost, "/webui/api/restart", map[string]string{}, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated restart status = %d", response.Code)
	}

	cookie := configuredSession(t, server)
	response = request(t, server, http.MethodPost, "/webui/api/restart", map[string]string{}, cookie)
	if response.Code != http.StatusAccepted {
		t.Fatalf("restart status = %d", response.Code)
	}
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart was not requested")
	}
}

func testServer(t *testing.T, restart RestartFunc) (*Server, *config.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Default(config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"})
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	cfg.Auth.ClaimCode = "01234567"
	cfg.Auth.BearerToken = "bearer-secret"
	if err := writeConfig(path, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store, err := config.Open(path)
	if err != nil {
		t.Fatalf("open config: %v", err)
	}
	return New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), restart), store
}

func configuredSession(t *testing.T, server *Server) *http.Cookie {
	t.Helper()
	bootstrap := request(t, server, http.MethodPost, "/webui/api/login", loginRequest{Username: defaultUsername, Password: defaultPassword}, nil)
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("bootstrap login status = %d", bootstrap.Code)
	}
	cookie := sessionCookieFrom(t, bootstrap)
	changed := request(t, server, http.MethodPost, "/webui/api/password", passwordRequest{Password: "correct-horse"}, cookie)
	if changed.Code != http.StatusOK {
		t.Fatalf("bootstrap password status = %d", changed.Code)
	}
	return passwordSession(t, server, "correct-horse")
}

func passwordSession(t *testing.T, server *Server, password string) *http.Cookie {
	t.Helper()
	response := request(t, server, http.MethodPost, "/webui/api/login", loginRequest{Username: defaultUsername, Password: password}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("configured login status = %d", response.Code)
	}
	return sessionCookieFrom(t, response)
}

func writeConfig(path string, cfg config.Config) error {
	if _, err := config.Create(path, config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"}); err != nil {
		return err
	}
	store, err := config.Open(path)
	if err != nil {
		return err
	}
	return store.Update(func(current *config.Config) error {
		*current = cfg
		return nil
	})
}

func request(t *testing.T, server *Server, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func sessionCookieFrom(t *testing.T, response *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie
		}
	}
	t.Fatal("session cookie not found")
	return nil
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(dst); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

package webui

import (
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/system"
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
)

func TestBootstrapRequiresPasswordChange(t *testing.T) {
	t.Parallel()

	server, _ := testServer(t, nil)

	response := request(t, server, http.MethodPost, "/webui/api/login", loginRequest{Username: defaultUsername, Password: defaultPassword}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap login status = %d", response.Code)
	}

	cookie := sessionCookieFrom(t, response)

	response = request(t, server, http.MethodGet, "/webui/api/config/entrypoints", nil, cookie)
	if response.Code != http.StatusForbidden {
		t.Fatalf("bootstrap config status = %d", response.Code)
	}

	response = request(t, server, http.MethodPost, "/webui/api/bootstrap/account", passwordRequest{Username: "admin", Password: "correct-horse", PasswordConfirm: "correct-horse"}, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("password change status = %d: %s", response.Code, response.Body.String())
	}

	response = request(t, server, http.MethodGet, "/webui/api/config/entrypoints", nil, cookie)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("old bootstrap session status = %d", response.Code)
	}
}

func TestBootstrapRejectsMismatchedPasswordConfirmation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Default(config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	server := New(store, auth.NewStore(store), slog.New(slog.DiscardHandler), nil, nil, func(string) error { return nil })
	login := request(t, server, http.MethodPost, "/webui/api/login", loginRequest{Username: defaultUsername, Password: defaultPassword}, nil)
	response := request(t, server, http.MethodPost, "/webui/api/bootstrap/account", passwordRequest{
		Username:        "admin",
		Password:        "correct-horse",
		PasswordConfirm: "different-horse",
	}, sessionCookieFrom(t, login))

	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "password confirmation does not match") {
		t.Fatalf("mismatched confirmation response = %d: %s", response.Code, response.Body.String())
	}
	if store.Snapshot().WebUI.AdminPasswordHash != "" || store.Snapshot().Auth.PairingState != config.PairingStateSetupRequired {
		t.Fatalf("mismatched confirmation changed bootstrap state: %#v", store.Snapshot())
	}
}

func TestBootstrapOwnerSetupIssuesClaimCodeAndCanIssueRepairCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Default(config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	authStore := auth.NewStore(store)
	server := New(store, authStore, slog.New(slog.DiscardHandler), nil, nil, func(string) error { return nil })

	login := request(t, server, http.MethodPost, "/webui/api/login", loginRequest{Username: defaultUsername, Password: defaultPassword}, nil)
	setup := request(t, server, http.MethodPost, "/webui/api/bootstrap/account", passwordRequest{Username: "admin", Password: "correct-horse", PasswordConfirm: "correct-horse"}, sessionCookieFrom(t, login))
	claimCode, err := authStore.InitialClaimCode()
	if err != nil {
		t.Fatal(err)
	}
	if setup.Code != http.StatusOK || !config.ValidClaimCode(claimCode) || strings.Contains(setup.Body.String(), "claim_code") {
		t.Fatalf("owner setup response = %d: %s", setup.Code, setup.Body.String())
	}
	pairing := request(t, server, http.MethodGet, "/webui/api/config/homeassistant", nil, passwordSession(t, server, "correct-horse"))
	var pairingStatus struct {
		ClaimCode    string `json:"claim_code"`
		PairingState string `json:"pairing_state"`
	}
	decodeResponse(t, pairing, &pairingStatus)
	if pairing.Code != http.StatusOK || pairingStatus.PairingState != string(config.PairingStateClaimable) || pairingStatus.ClaimCode != claimCode {
		t.Fatalf("pairing status = %d: %s", pairing.Code, pairing.Body.String())
	}

	if _, err := authStore.RotateBearer(); err != nil {
		t.Fatal(err)
	}
	repair := request(t, server, http.MethodPost, "/webui/api/config/homeassistant/recovery-code", map[string]string{}, passwordSession(t, server, "correct-horse"))
	if repair.Code != http.StatusOK || !strings.Contains(repair.Body.String(), "repair_code") {
		t.Fatalf("repair code response = %d: %s", repair.Code, repair.Body.String())
	}
}

func TestConfigIsRedactedAndSavedThroughStore(t *testing.T) {
	t.Parallel()

	server, store := testServer(t, nil)
	cookie := configuredSession(t, server)

	response := request(t, server, http.MethodGet, "/webui/api/config/entrypoints", nil, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("config status = %d", response.Code)
	}

	if strings.Contains(response.Body.String(), "claim_code") || strings.Contains(response.Body.String(), "bearer_token") || strings.Contains(response.Body.String(), "session_secret") {
		t.Fatalf("redacted config contains secret: %s", response.Body.String())
	}

	if !strings.Contains(response.Body.String(), `"id":"main"`) || !strings.Contains(response.Body.String(), `"devaddr":"20"`) || !strings.Contains(response.Body.String(), `"capabilities":{"stream":true,"unlock":true,"ring":true}`) {
		t.Fatalf("entrypoint JSON contract = %s", response.Body.String())
	}
	var editable intercomConfig
	decodeResponse(t, response, &editable)
	if len(editable.Entrypoints) != 1 || editable.Entrypoints[0].ID != "main" || editable.Entrypoints[0].DevAddr != "20" {
		t.Fatalf("default entrypoint = %#v", editable.Entrypoints)
	}
	editable.Entrypoints[0].Label = "Updated Gate"

	response = request(t, server, http.MethodPut, "/webui/api/config/entrypoints", editable, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("save config status = %d: %s", response.Code, response.Body.String())
	}

	if store.Snapshot().Companion.Entrypoints[0].Label != "Updated Gate" {
		t.Fatalf("saved entrypoint label = %q", store.Snapshot().Companion.Entrypoints[0].Label)
	}

	if store.Snapshot().Auth.BearerTokenHash == "" || store.Snapshot().Auth.PairingState != config.PairingStateClaimed {
		t.Fatal("config save changed auth secrets")
	}
}

func TestHomeKitConfigExposesPairingDetailsAndSavesBridgeSettings(t *testing.T) {
	t.Parallel()

	server, store := testServer(t, nil)
	cookie := configuredSession(t, server)

	response := request(t, server, http.MethodGet, "/webui/api/config/homekit", nil, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("get homekit config status = %d: %s", response.Code, response.Body.String())
	}
	var current editableHomeKit
	decodeResponse(t, response, &current)
	if current.PIN == "" || current.Name == "" || current.Port == 0 {
		t.Fatalf("homekit config = %#v, want PIN, name, and port", current)
	}

	response = request(t, server, http.MethodPut, "/webui/api/config/homekit", editableHomeKit{
		Enabled: true,
		Name:    "Front Door",
		Port:    12345,
	}, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("save homekit config status = %d: %s", response.Code, response.Body.String())
	}
	if got := store.Snapshot().HomeKit; !got.Enabled || got.Name != "Front Door" || got.Port != 12345 {
		t.Fatalf("saved homekit config = %#v", got)
	}
}

func TestStatusIncludesRuntimeMetrics(t *testing.T) {
	server, _ := testServer(t, nil)
	cookie := configuredSession(t, server)

	response := request(t, server, http.MethodGet, "/webui/api/management/diagnostics", nil, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var payload map[string]any
	decodeResponse(t, response, &payload)
	if _, ok := payload["uptime_seconds"].(float64); !ok {
		t.Fatalf("uptime_seconds missing or invalid: %#v", payload["uptime_seconds"])
	}
	if _, ok := payload["free_ram_kb"].(float64); !ok {
		t.Fatalf("free_ram_kb missing or invalid: %#v", payload["free_ram_kb"])
	}
}

func TestSessionIncludesBuildInfoAndUpdateStatusEndpoint(t *testing.T) {
	t.Parallel()

	server, _ := testServer(t, nil)
	server.SetUpdate(fakeUpdateProvider{status: system.UpdateStatus{Enabled: true, CurrentVersion: "v1.2.3", Stage: "idle"}})
	cookie := configuredSession(t, server)

	session := request(t, server, http.MethodGet, "/webui/api/session", nil, cookie)
	var sessionBody map[string]any
	decodeResponse(t, session, &sessionBody)
	if sessionBody["version"] == "" || sessionBody["git_sha"] == "" {
		t.Fatalf("session build info = %#v", sessionBody)
	}

	response := request(t, server, http.MethodGet, "/webui/api/management/update", nil, cookie)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"current_version":"v1.2.3"`) {
		t.Fatalf("update status = %d: %s", response.Code, response.Body.String())
	}
}

func TestSessionDoesNotExposeOwnerOrBuildMetadataWithoutAuthentication(t *testing.T) {
	t.Parallel()

	server, _ := testServer(t, nil)
	response := request(t, server, http.MethodGet, "/webui/api/session", nil, nil)
	var sessionBody map[string]any
	decodeResponse(t, response, &sessionBody)

	for _, field := range []string{"username", "version", "git_sha"} {
		if _, exposed := sessionBody[field]; exposed {
			t.Errorf("unauthenticated session exposed %q", field)
		}
	}
}

func TestPasswordChangeRotatesSecretAndInvalidatesEverySession(t *testing.T) {
	t.Parallel()

	server, store := testServer(t, nil)
	first := configuredSession(t, server)
	second := passwordSession(t, server, "correct-horse")
	previousSecret := store.Snapshot().WebUI.SessionSecret

	response := request(t, server, http.MethodPost, "/webui/api/admin/account", passwordRequest{Username: "admin", CurrentPassword: "correct-horse", Password: "new-password"}, first)
	if response.Code != http.StatusOK {
		t.Fatalf("password change status = %d: %s", response.Code, response.Body.String())
	}

	if store.Snapshot().WebUI.SessionSecret == previousSecret {
		t.Fatal("session secret did not rotate")
	}

	response = request(t, server, http.MethodGet, "/webui/api/config/entrypoints", nil, second)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("old session status = %d", response.Code)
	}
}

func TestRestartRequiresConfiguredSession(t *testing.T) {
	t.Parallel()

	restarted := make(chan struct{}, 1)
	server, _ := testServer(t, func(context.Context) error {
		restarted <- struct{}{}
		return nil
	})

	response := request(t, server, http.MethodPost, "/webui/api/admin/restart", map[string]string{}, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated restart status = %d", response.Code)
	}

	cookie := configuredSession(t, server)

	response = request(t, server, http.MethodPost, "/webui/api/admin/restart", map[string]bool{"confirm": true}, cookie)
	if response.Code != http.StatusAccepted {
		t.Fatalf("restart status = %d", response.Code)
	}

	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart was not requested")
	}
}

func TestUpdateInstallRequiresConfiguredSessionAndInvokesUpdater(t *testing.T) {
	t.Parallel()

	installed := make(chan struct{}, 1)
	server, _ := testServer(t, nil)
	server.SetUpdate(fakeUpdateProvider{install: func(context.Context) (system.UpdateStatus, error) {
		installed <- struct{}{}
		return system.UpdateStatus{RestartRequired: true}, nil
	}})

	response := request(t, server, http.MethodPost, "/webui/api/management/update", map[string]string{}, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated install status = %d", response.Code)
	}

	response = request(t, server, http.MethodPost, "/webui/api/management/update", map[string]string{}, configuredSession(t, server))
	if response.Code != http.StatusAccepted {
		t.Fatalf("install status = %d: %s", response.Code, response.Body.String())
	}
	select {
	case <-installed:
	case <-time.After(time.Second):
		t.Fatal("update install was not requested")
	}
}

func TestUpdateCheckRequiresConfiguredSessionAndInvokesUpdater(t *testing.T) {
	t.Parallel()

	checked := make(chan struct{}, 1)
	server, _ := testServer(t, nil)
	server.SetUpdate(fakeUpdateProvider{check: func(context.Context) (system.UpdateStatus, error) {
		checked <- struct{}{}
		return system.UpdateStatus{CurrentVersion: "v1.2.3", LatestVersion: "v1.2.4", UpdateAvailable: true, Stage: "available"}, nil
	}})

	response := request(t, server, http.MethodPost, "/webui/api/management/update/check", map[string]string{}, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated check status = %d", response.Code)
	}

	response = request(t, server, http.MethodPost, "/webui/api/management/update/check", map[string]string{}, configuredSession(t, server))
	if response.Code != http.StatusOK {
		t.Fatalf("check status = %d: %s", response.Code, response.Body.String())
	}
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("update check was not requested")
	}
}

func testServer(t *testing.T, restart RestartFunc) (*Server, *config.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")

	cfg, err := config.Default(config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"})
	if err != nil {
		t.Fatalf("default config: %v", err)
	}

	if err := writeConfig(path, cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store, err := config.Open(path)
	if err != nil {
		t.Fatalf("open config: %v", err)
	}

	authStore := auth.NewStore(store)
	if err := store.Update(func(cfg *config.Config) error {
		cfg.WebUI.SessionSecret = "test-session-secret"
		return nil
	}); err != nil {
		t.Fatalf("set session secret: %v", err)
	}
	if _, err := authStore.StartInitialClaim(); err != nil {
		t.Fatalf("start initial claim: %v", err)
	}
	if _, err := authStore.RotateBearer(); err != nil {
		t.Fatalf("create bearer: %v", err)
	}
	return New(store, authStore, slog.New(slog.DiscardHandler), restart, nil, func(string) error { return nil }), store
}

func configuredSession(t *testing.T, server *Server) *http.Cookie {
	t.Helper()

	bootstrap := request(t, server, http.MethodPost, "/webui/api/login", loginRequest{Username: defaultUsername, Password: defaultPassword}, nil)
	if bootstrap.Code != http.StatusOK {
		t.Fatalf("bootstrap login status = %d", bootstrap.Code)
	}

	cookie := sessionCookieFrom(t, bootstrap)

	changed := request(t, server, http.MethodPost, "/webui/api/bootstrap/account", passwordRequest{Username: "admin", Password: "correct-horse", PasswordConfirm: "correct-horse"}, cookie)
	if changed.Code != http.StatusOK {
		t.Fatalf("bootstrap password status = %d", changed.Code)
	}

	return passwordSession(t, server, "correct-horse")
}

func passwordSession(t *testing.T, server *Server, password string) *http.Cookie {
	t.Helper()

	response := request(t, server, http.MethodPost, "/webui/api/login", loginRequest{Username: "admin", Password: password}, nil)
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

	request := httptest.NewRequestWithContext(context.Background(), method, path, reader)
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

type fakeUpdateProvider struct {
	status  system.UpdateStatus
	check   func(context.Context) (system.UpdateStatus, error)
	install func(context.Context) (system.UpdateStatus, error)
}

func (f fakeUpdateProvider) Status(context.Context) (system.UpdateStatus, error) {
	return f.status, nil
}

func (f fakeUpdateProvider) Check(ctx context.Context) (system.UpdateStatus, error) {
	if f.check == nil {
		return f.status, nil
	}
	return f.check(ctx)
}

func (f fakeUpdateProvider) Install(ctx context.Context) (system.UpdateStatus, error) {
	if f.install == nil {
		return f.status, nil
	}
	return f.install(ctx)
}

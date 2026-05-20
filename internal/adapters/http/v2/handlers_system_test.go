package v2

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/services/control"
	"bticino-go-companion/internal/services/events"
	"bticino-go-companion/internal/services/state"
	"bticino-go-companion/internal/services/systemcontrol"
	"bticino-go-companion/internal/services/trace"
	"bticino-go-companion/internal/services/update"
	"bticino-go-companion/internal/system"
)

type systemManagerStub struct {
	rebootCalls  int
	restartCalls []string
	statusCalls  []string
	rebootErr    error
	restartErr   error
	statusErr    error
	statusByName map[string]system.ServiceStatus
}

func (s *systemManagerStub) RebootHost(context.Context) error {
	s.rebootCalls++
	return s.rebootErr
}

func (s *systemManagerStub) Status(_ context.Context, serviceName string) (system.ServiceStatus, error) {
	s.statusCalls = append(s.statusCalls, serviceName)
	if s.statusErr != nil {
		return system.ServiceStatus{}, s.statusErr
	}
	if status, ok := s.statusByName[serviceName]; ok {
		return status, nil
	}
	return system.ServiceStatus{Name: serviceName, Running: true}, nil
}

func (s *systemManagerStub) Restart(_ context.Context, serviceName string) error {
	s.restartCalls = append(s.restartCalls, serviceName)
	return s.restartErr
}

func newAuthedRouterWithDeps(t *testing.T, cfg config.Config, systemSvc *systemcontrol.Service, updateMgr *update.Manager) (*Router, string) {
	t.Helper()
	authStore, token := newClaimedAuth(t)
	projector := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	ctrl := control.New(projector.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, audioNoop{}, voicemailNoop{}, nil)
	r := NewRouter(cfg, authStore, projector, ctrl, events.New(32), newTestRuntimeStatus(), trace.New(32), systemSvc, updateMgr, nil)
	return r, token
}

func decodeResponseBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body failed: %v body=%s", err, rr.Body.String())
	}
	return out
}

func TestSystemControlHandlersSuccess(t *testing.T) {
	cfg := config.Default()
	cfg.SystemRebootEnabled = true
	cfg.SystemServices = map[string]config.SystemServiceConfig{
		"dropbear": {Enabled: true, Exposed: true},
	}
	manager := &systemManagerStub{
		statusByName: map[string]system.ServiceStatus{
			"dropbear": {Name: "dropbear", Running: true},
		},
	}
	systemSvc := systemcontrol.New(manager, cfg.SystemRebootEnabled, cfg.SystemServices)
	r, token := newAuthedRouterWithDeps(t, cfg, systemSvc, nil)

	req := authReq(http.MethodGet, "/api/v2/control/system/services", token)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("services expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodGet, "/api/v2/control/system/services/dropbear/status", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("service status expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	body := decodeResponseBody(t, rr)
	service, _ := body["service"].(map[string]any)
	if strings.ToLower(strings.TrimSpace(service["name"].(string))) != "dropbear" {
		t.Fatalf("unexpected service status body: %+v", body)
	}

	req = authReq(http.MethodPost, "/api/v2/control/system/services/dropbear/restart", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("service restart expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodPost, "/api/v2/control/system/reboot", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("system reboot expected 202, got %d body=%s", rr.Code, rr.Body.String())
	}
	if manager.rebootCalls != 1 {
		t.Fatalf("expected 1 reboot call, got %d", manager.rebootCalls)
	}
	if len(manager.restartCalls) != 1 || manager.restartCalls[0] != "dropbear" {
		t.Fatalf("unexpected restart calls: %+v", manager.restartCalls)
	}
}

func TestSystemControlHandlersErrorMapping(t *testing.T) {
	cfg := config.Default()
	cfg.SystemRebootEnabled = false
	cfg.SystemServices = map[string]config.SystemServiceConfig{
		"dropbear": {Enabled: true, Exposed: true},
		"dbus":     {Enabled: false, Exposed: true},
	}
	manager := &systemManagerStub{}
	systemSvc := systemcontrol.New(manager, cfg.SystemRebootEnabled, cfg.SystemServices)
	r, token := newAuthedRouterWithDeps(t, cfg, systemSvc, nil)

	req := authReq(http.MethodPost, "/api/v2/control/system/reboot", token)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected reboot disabled 409, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodPost, "/api/v2/control/system/services/dbus/restart", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected disabled service 409, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodGet, "/api/v2/control/system/services/unknown/status", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected not exposed service 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSystemControlHandlersUnavailable(t *testing.T) {
	cfg := config.Default()
	r, token := newAuthedRouterWithDeps(t, cfg, nil, nil)

	req := authReq(http.MethodGet, "/api/v2/control/system/services", token)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when system service nil, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUpdateHandlersLifecycle(t *testing.T) {
	cfg := config.Default()
	cfg.Version = "v1.0.0"
	cfg.SystemUpdateEnabled = true
	cfg.SystemUpdateExposed = true
	cfg.SystemUpdateAllowRollback = true
	cfg.DataDir = filepath.Join(t.TempDir(), "companion")

	currentPath := cfg.UpdateBinCurrentPath()
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o755); err != nil {
		t.Fatalf("mkdir current dir: %v", err)
	}
	if err := os.WriteFile(currentPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("write current binary: %v", err)
	}
	candidatePath := filepath.Join(t.TempDir(), "candidate.bin")
	if err := os.WriteFile(candidatePath, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	updateMgr := update.NewManager(cfg, log.New(io.Discard, "", 0), nil)
	updateMgr.SetRestartForTest(func() error { return nil })
	r, token := newAuthedRouterWithDeps(t, cfg, nil, updateMgr)

	checkBody := `{"available_version":"v1.1.0","artifact_path":"` + candidatePath + `"}`
	req := authReq(http.MethodPost, "/api/v2/control/system/update/check", token)
	req.Body = io.NopCloser(strings.NewReader(checkBody))
	req.ContentLength = int64(len(checkBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update check expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodPost, "/api/v2/control/system/update/apply", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("update apply expected 202, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodPost, "/api/v2/control/system/update/rollback", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("update rollback expected 202, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodGet, "/api/v2/control/system/update/status", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update status expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUpdateHandlersGatesAndBadBody(t *testing.T) {
	cfg := config.Default()
	cfg.SystemUpdateEnabled = true
	cfg.SystemUpdateExposed = false
	r, token := newAuthedRouterWithDeps(t, cfg, nil, nil)

	req := authReq(http.MethodGet, "/api/v2/control/system/update/status", token)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected update not exposed 404, got %d body=%s", rr.Code, rr.Body.String())
	}

	cfg.SystemUpdateExposed = true
	cfg.SystemUpdateAllowRollback = false
	r, token = newAuthedRouterWithDeps(t, cfg, nil, nil)

	req = authReq(http.MethodGet, "/api/v2/control/system/update/status", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected update unavailable 503, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodPost, "/api/v2/control/system/update/check", token)
	req.Body = io.NopCloser(strings.NewReader("{"))
	req.ContentLength = 1
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 before body parse when update manager missing, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodPost, "/api/v2/control/system/update/apply", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected update unavailable 503, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodPost, "/api/v2/control/system/update/rollback", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected rollback disabled 409, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestDecodeOptionalUpdateJSONBody(t *testing.T) {
	var dst struct {
		Value string `json:"value"`
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v2/control/system/update/check", nil)
	hasBody, err := decodeOptionalUpdateJSONBody(req, &dst)
	if err != nil || hasBody {
		t.Fatalf("expected no body for nil body, got hasBody=%v err=%v", hasBody, err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v2/control/system/update/check", strings.NewReader(" \n\t "))
	req.ContentLength = 4
	hasBody, err = decodeOptionalUpdateJSONBody(req, &dst)
	if err != nil || hasBody {
		t.Fatalf("expected no body for whitespace payload, got hasBody=%v err=%v", hasBody, err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v2/control/system/update/check", strings.NewReader("{"))
	req.ContentLength = 1
	if _, err := decodeOptionalUpdateJSONBody(req, &dst); err == nil {
		t.Fatal("expected invalid json error")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v2/control/system/update/check", strings.NewReader(`{"value":"ok"}`))
	req.ContentLength = int64(len(`{"value":"ok"}`))
	hasBody, err = decodeOptionalUpdateJSONBody(req, &dst)
	if err != nil || !hasBody || dst.Value != "ok" {
		t.Fatalf("expected parsed body, got hasBody=%v dst=%+v err=%v", hasBody, dst, err)
	}
}

func TestVoicemailHandlers(t *testing.T) {
	base := t.TempDir()
	messageDir := filepath.Join(base, "1001")
	if err := os.MkdirAll(messageDir, 0o755); err != nil {
		t.Fatalf("mkdir message dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(messageDir, "msg_info.ini"), []byte("[Message Information]\nUnixTime=100\nRead=1\n"), 0o644); err != nil {
		t.Fatalf("write message info: %v", err)
	}
	if err := os.WriteFile(filepath.Join(messageDir, "aswm.jpg"), []byte("jpg"), 0o644); err != nil {
		t.Fatalf("write thumbnail: %v", err)
	}

	cfg := config.Default()
	cfg.VoicemailMessagesDir = base
	r, token := newAuthedRouterWithDeps(t, cfg, nil, nil)

	req := authReq(http.MethodGet, "/api/v2/voicemail/messages", token)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("voicemail list expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodGet, "/api/v2/voicemail/messages/1001/aswm.jpg", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("voicemail asset expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/jpeg") {
		t.Fatalf("unexpected content type: %s", ct)
	}

	req = authReq(http.MethodGet, "/api/v2/voicemail/messages/1001/not-valid.bin", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid asset expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

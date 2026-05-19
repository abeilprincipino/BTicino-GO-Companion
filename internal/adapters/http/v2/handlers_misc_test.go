package v2

import (
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/services/control"
	"bticino-go-companion/internal/services/events"
	"bticino-go-companion/internal/services/state"
	"bticino-go-companion/internal/services/trace"
)

type unlockErrDriver struct{ err error }

func (d unlockErrDriver) Unlock(context.Context, string) error { return d.err }

type streamErrDriver struct {
	startErr error
	stopErr  error
}

func (d streamErrDriver) StartForEntrypoint(context.Context, string, string) error { return d.startErr }
func (d streamErrDriver) StopForEntrypoint(context.Context, string) error           { return d.stopErr }

type callErrDriver struct {
	answerErr error
	hangupErr error
}

func (d callErrDriver) Answer(context.Context) error { return d.answerErr }
func (d callErrDriver) Hangup(context.Context) error { return d.hangupErr }

type audioErrDriver struct {
	muteErr   error
	unmuteErr error
}

func (d audioErrDriver) Mute(context.Context) error   { return d.muteErr }
func (d audioErrDriver) Unmute(context.Context) error { return d.unmuteErr }

type voicemailErrDriver struct {
	enableErr  error
	disableErr error
}

func (d voicemailErrDriver) VoicemailEnable(context.Context) error  { return d.enableErr }
func (d voicemailErrDriver) VoicemailDisable(context.Context) error { return d.disableErr }

func newAuthedRouterWithControl(t *testing.T, cfg config.Config, controlSvc *control.Service) (*Router, string) {
	t.Helper()
	authStore, token := newClaimedAuth(t)
	projector := state.NewProjector(cfg.Entrypoints)
	r := NewRouter(cfg, authStore, projector, controlSvc, events.New(32), newTestRuntimeStatus(), trace.New(32), nil, nil)
	return r, token
}

func TestControlEntrypointEndpointsAndErrorMappings(t *testing.T) {
	cfg := config.Default()
	cfg.Entrypoints = []entrypoint.Model{
		{ID: "main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true},
		{ID: "aux", DevAddr: "21", HasStream: false, HasUnlock: false, HasRing: true},
	}
	controlSvc := control.New(
		cfg.Entrypoints,
		streamErrDriver{},
		unlockErrDriver{},
		callErrDriver{},
		audioErrDriver{},
		voicemailErrDriver{},
		nil,
	)
	r, token := newAuthedRouterWithControl(t, cfg, controlSvc)

	tests := []struct {
		name   string
		method string
		path   string
		code   int
	}{
		{name: "unlock success", method: http.MethodPost, path: "/api/v2/control/entrypoints/main/unlock", code: http.StatusOK},
		{name: "stream start success", method: http.MethodPost, path: "/api/v2/control/entrypoints/main/stream/start", code: http.StatusOK},
		{name: "stream stop success", method: http.MethodPost, path: "/api/v2/control/entrypoints/main/stream/stop", code: http.StatusOK},
		{name: "entrypoint missing", method: http.MethodPost, path: "/api/v2/control/entrypoints/missing/unlock", code: http.StatusNotFound},
		{name: "unlock capability disabled", method: http.MethodPost, path: "/api/v2/control/entrypoints/aux/unlock", code: http.StatusConflict},
		{name: "stream capability disabled", method: http.MethodPost, path: "/api/v2/control/entrypoints/aux/stream/start", code: http.StatusConflict},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := authReq(tc.method, tc.path, token)
			rr := httptest.NewRecorder()
			r.Handler().ServeHTTP(rr, req)
			if rr.Code != tc.code {
				t.Fatalf("expected %d, got %d body=%s", tc.code, rr.Code, rr.Body.String())
			}
		})
	}

	controlSvc = control.New(cfg.Entrypoints, streamErrDriver{}, unlockErrDriver{}, nil, nil, nil, nil)
	r, token = newAuthedRouterWithControl(t, cfg, controlSvc)
	for _, tc := range []struct {
		path string
		code int
	}{
		{path: "/api/v2/control/call/answer", code: http.StatusConflict},
		{path: "/api/v2/control/call/hangup", code: http.StatusConflict},
		{path: "/api/v2/control/audio/mute", code: http.StatusConflict},
		{path: "/api/v2/control/voicemail/enable", code: http.StatusConflict},
	} {
		req := authReq(http.MethodPost, tc.path, token)
		rr := httptest.NewRecorder()
		r.Handler().ServeHTTP(rr, req)
		if rr.Code != tc.code {
			t.Fatalf("path %s: expected %d, got %d body=%s", tc.path, tc.code, rr.Code, rr.Body.String())
		}
	}
}

func TestTraceAndReadEndpoints(t *testing.T) {
	r, token := newAuthedRouter(t)

	req := authReq(http.MethodGet, "/api/v2/health", token)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("health expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodGet, "/api/v2/capabilities", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("capabilities expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodGet, "/api/v2/entrypoints", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("entrypoints expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodGet, "/api/v2/trace/openwebnet/stream", token)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	cancel()
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("trace stream expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAuthStatusRotateRevokeRepairReset(t *testing.T) {
	authStore, token := newClaimedAuth(t)
	cfg := config.Default()
	p := state.NewProjector(cfg.Entrypoints)
	ctrl := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, audioNoop{}, voicemailNoop{}, nil)
	r := NewRouter(cfg, authStore, p, ctrl, events.New(8), newTestRuntimeStatus(), trace.New(8), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/auth/status", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("auth status without token expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodGet, "/api/v2/auth/status", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("auth status with token expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodPost, "/api/v2/auth/rotate", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("auth rotate expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var rotate map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &rotate); err != nil {
		t.Fatalf("decode rotate body: %v", err)
	}
	rotatedToken, _ := rotate["access_token"].(string)
	if strings.TrimSpace(rotatedToken) == "" {
		t.Fatalf("expected rotated access token in body: %v", rotate)
	}

	req = authReq(http.MethodPost, "/api/v2/auth/revoke", rotatedToken)
	req.Body = io.NopCloser(strings.NewReader(`{"key_id":"does-not-exist"}`))
	req.ContentLength = int64(len(`{"key_id":"does-not-exist"}`))
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("auth revoke unknown key expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodPost, "/api/v2/admin/issue-repair-code", rotatedToken)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("issue repair code expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var issue map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &issue); err != nil {
		t.Fatalf("decode issue-repair-code body: %v", err)
	}
	repairCode, _ := issue["repair_code"].(string)
	if strings.TrimSpace(repairCode) == "" {
		t.Fatalf("expected non-empty repair code in body: %v", issue)
	}

	req = authReq(http.MethodPost, "/api/v2/admin/reset-claim", rotatedToken)
	resetBody := `{"repair_code":"` + repairCode + `"}`
	req.Body = io.NopCloser(strings.NewReader(resetBody))
	req.ContentLength = int64(len(resetBody))
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("reset claim expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAuthStatusWhenNeedsClaim(t *testing.T) {
	store, err := auth.NewStore(filepath.Join(t.TempDir(), "config.json"), "abcd-1234", "C300X", "00:11:22:33:44:55")
	if err != nil {
		t.Fatalf("new auth store: %v", err)
	}
	cfg := config.Default()
	p := state.NewProjector(cfg.Entrypoints)
	ctrl := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, audioNoop{}, voicemailNoop{}, nil)
	r := NewRouter(cfg, store, p, ctrl, events.New(8), newTestRuntimeStatus(), trace.New(8), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/auth/status", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("auth status expected 200 when claim needed, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHelperParsers(t *testing.T) {
	if got := bearerToken("Bearer token-1"); got != "token-1" {
		t.Fatalf("unexpected bearer token parse: %q", got)
	}
	if got := bearerToken("basic token-1"); got != "" {
		t.Fatalf("expected empty token for non-bearer header, got %q", got)
	}

	if got := rtspPort(":9554"); got != 9554 {
		t.Fatalf("unexpected rtsp port parse for :9554 -> %d", got)
	}
	if got := rtspPort("invalid"); got != 8554 {
		t.Fatalf("expected fallback rtsp port for invalid address, got %d", got)
	}
}

type noFlushRecorder struct {
	header http.Header
	code   int
	body   strings.Builder
}

func (r *noFlushRecorder) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}

func (r *noFlushRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *noFlushRecorder) WriteHeader(statusCode int)  { r.code = statusCode }

func TestTraceAndEventsErrorBranches(t *testing.T) {
	r := &Router{}
	req := httptest.NewRequest(http.MethodGet, "/api/v2/events", nil)
	rr := httptest.NewRecorder()
	r.handleEventsSSE(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected events unavailable 503, got %d", rr.Code)
	}

	r.events = events.New(8)
	noFlush := &noFlushRecorder{}
	r.handleEventsSSE(noFlush, req)
	if noFlush.code != http.StatusInternalServerError {
		t.Fatalf("expected sse unsupported 500, got %d", noFlush.code)
	}

	rr = httptest.NewRecorder()
	r.handleOpenWebNetTrace(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected trace unavailable 503, got %d", rr.Code)
	}

	r.trace = trace.New(8)
	noFlush = &noFlushRecorder{}
	r.handleOpenWebNetTraceStream(noFlush, req)
	if noFlush.code != http.StatusInternalServerError {
		t.Fatalf("expected trace stream unsupported 500, got %d", noFlush.code)
	}
}

func TestDecodeRequiredJSONBody(t *testing.T) {
	var dst map[string]any
	if err := decodeRequiredJSONBody(nil, &dst); err == nil {
		t.Fatal("expected error for nil request")
	}

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	if err := decodeRequiredJSONBody(req, &dst); err == nil {
		t.Fatal("expected error for empty body")
	}

	req = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"a":1}{"b":2}`))
	if err := decodeRequiredJSONBody(req, &dst); err == nil {
		t.Fatal("expected error for multiple json values")
	}

	req = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"a":1}`))
	if err := decodeRequiredJSONBody(req, &dst); err != nil {
		t.Fatalf("expected valid json decode, got %v", err)
	}
}

func TestAuthHandlerErrorBranches(t *testing.T) {
	store, err := auth.NewStore(filepath.Join(t.TempDir(), "config.json"), "abcd-1234", "C300X", "00:11:22:33:44:55")
	if err != nil {
		t.Fatalf("new auth store: %v", err)
	}
	r := &Router{auth: store}

	// Pair claim with invalid JSON.
	req := httptest.NewRequest(http.MethodPost, "/api/v2/pair/claim", strings.NewReader("{"))
	rr := httptest.NewRecorder()
	r.handlePairClaim(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for invalid claim json, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Claim with invalid challenge.
	req = httptest.NewRequest(http.MethodPost, "/api/v2/pair/claim", strings.NewReader(`{"challenge_id":"x","nonce":"y","claim_code":"abcd-1234"}`))
	rr = httptest.NewRecorder()
	r.handlePairClaim(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized for invalid challenge, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Start challenge, then claim with wrong code.
	ch, err := store.StartChallenge()
	if err != nil {
		t.Fatalf("start challenge: %v", err)
	}
	body := `{"challenge_id":"` + ch.ID + `","nonce":"` + ch.Nonce + `","claim_code":"bad-code"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v2/pair/claim", strings.NewReader(body))
	rr = httptest.NewRecorder()
	r.handlePairClaim(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized for invalid claim code, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Issue repair code should fail before claim.
	req = httptest.NewRequest(http.MethodPost, "/api/v2/admin/issue-repair-code", nil)
	rr = httptest.NewRecorder()
	r.handleIssueRepairCode(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected repair not allowed 409, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Reset claim invalid body.
	req = httptest.NewRequest(http.MethodPost, "/api/v2/admin/reset-claim", strings.NewReader(`{}`))
	rr = httptest.NewRecorder()
	r.handleResetClaim(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for missing repair code, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestPairChallengeAlreadyClaimed(t *testing.T) {
	store, token := newClaimedAuth(t)
	r := &Router{auth: store}

	req := httptest.NewRequest(http.MethodPost, "/api/v2/pair/challenge", nil)
	rr := httptest.NewRecorder()
	r.handlePairChallenge(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected already claimed conflict, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Use claim token to revoke with missing key id body.
	req = authReq(http.MethodPost, "/api/v2/auth/revoke", token)
	req.Body = ioutil.NopCloser(strings.NewReader(`{}`))
	req.ContentLength = 2
	rr = httptest.NewRecorder()
	r.handleAuthRevoke(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request for missing revoke key_id, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Claim status with invalid bearer when claimed.
	req = httptest.NewRequest(http.MethodGet, "/api/v2/auth/status", nil)
	req.Header.Set("Authorization", "Bearer invalid")
	rr = httptest.NewRecorder()
	r.handleAuthStatus(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized for invalid bearer on claimed status, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestBearerWrapperBranches(t *testing.T) {
	r := &Router{}
	nextCalled := false
	handler := r.withBearer(func(http.ResponseWriter, *http.Request) { nextCalled = true })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusServiceUnavailable || nextCalled {
		t.Fatalf("expected auth unavailable and next not called, got code=%d next=%v", rr.Code, nextCalled)
	}

	store, token := newClaimedAuth(t)
	r.auth = store
	handler = r.withBearer(func(http.ResponseWriter, *http.Request) { nextCalled = true })

	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	rr = httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized for missing bearer, got %d", rr.Code)
	}

	nextCalled = false
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != 0 && rr.Code != http.StatusOK {
		t.Fatalf("unexpected code for valid bearer: %d", rr.Code)
	}
	if !nextCalled {
		t.Fatal("expected next handler called on valid bearer")
	}
}

func TestAuthRepairFlowWithExpiredCode(t *testing.T) {
	store, token := newClaimedAuth(t)
	r := &Router{auth: store}

	code, _, err := store.IssueRepairCode(1 * time.Nanosecond)
	if err != nil {
		t.Fatalf("issue repair code: %v", err)
	}
	time.Sleep(2 * time.Millisecond)

	req := authReq(http.MethodPost, "/api/v2/admin/reset-claim", token)
	body := `{"repair_code":"` + code + `"}`
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	rr := httptest.NewRecorder()
	r.handleResetClaim(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected expired repair code unauthorized, got %d body=%s", rr.Code, rr.Body.String())
	}
}

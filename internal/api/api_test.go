package api

import (
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/system"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

func TestServer_EnvelopesAndBearer(t *testing.T) {
	t.Parallel()

	server, store := newTestServer(t)

	tests := []struct {
		name, method, path string
		status             int
		ok                 bool
	}{
		{"health", "GET", "/api/v3/health", 200, true},
		{"method", "POST", "/api/v3/health", 405, false},
		{"missing", "GET", "/api/v3/missing", 404, false},
		{"state unauthorized", "GET", "/api/v3/state", 401, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request := httptest.NewRequestWithContext(context.Background(), test.method, test.path, nil)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)

			var body struct {
				OK    bool            `json:"ok"`
				Error json.RawMessage `json:"error"`
			}

			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}

			if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.OK != test.ok || (!test.ok && len(body.Error) == 0) {
				t.Fatalf("invalid envelope %s", response.Body.String())
			}
		})
	}

	token, err := auth.NewStore(store).RotateBearer()
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v3/state", nil)
	request.Header.Set("Authorization", "Bearer "+token)

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != 200 {
		t.Fatalf("authorized state = %d", response.Code)
	}
}

func TestServer_Pair(t *testing.T) {
	t.Parallel()

	server, store := newTestServer(t)
	challengeRequest := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/pair/challenge", nil)
	challengeRequest.RemoteAddr = "192.0.2.1:1234"
	challengeResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(challengeResponse, challengeRequest)

	var challenge struct {
		ChallengeID string `json:"challenge_id"`
	}
	if challengeResponse.Code != 201 || json.Unmarshal(challengeResponse.Body.Bytes(), &challenge) != nil {
		t.Fatalf("challenge response = %s", challengeResponse.Body.String())
	}

	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/pair/claim", strings.NewReader(`{"challenge_id":"`+challenge.ChallengeID+`","claim_code":"`+store.Snapshot().Auth.ClaimCode+`"}`))
	request.RemoteAddr = "192.0.2.1:1234"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	var claim struct {
		AccessToken string `json:"access_token"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &claim) != nil || claim.AccessToken == "" {
		t.Fatalf("claim response = %s", response.Body.String())
	}
}

func TestServer_RepairReset(t *testing.T) {
	t.Parallel()

	server, _ := newTestServer(t)
	token, err := server.auth.RotateBearer()
	if err != nil {
		t.Fatal(err)
	}

	issue := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/admin/issue-repair-code", nil)
	issue.Header.Set("Authorization", "Bearer "+token)
	issued := httptest.NewRecorder()
	server.Handler().ServeHTTP(issued, issue)
	var repair struct {
		RepairCode string `json:"repair_code"`
	}
	if issued.Code != http.StatusOK || json.Unmarshal(issued.Body.Bytes(), &repair) != nil || repair.RepairCode == "" {
		t.Fatalf("issue repair response = %s", issued.Body.String())
	}

	reset := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/admin/reset-claim", strings.NewReader(`{"repair_code":"`+repair.RepairCode+`"}`))
	reset.Header.Set("Authorization", "Bearer "+token)
	resetResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(resetResponse, reset)
	if resetResponse.Code != http.StatusOK {
		t.Fatalf("reset repair response = %s", resetResponse.Body.String())
	}
	if server.auth.ValidateBearer(token) {
		t.Fatal("reset repair did not revoke bearer")
	}
}

func TestParseMessage(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		message string
		wantErr bool
	}{{`{"type":"ping","id":"ping-1"}`, false}, {`{"type":"command","id":"state-1","action":"state.get"}`, true}, {`{"type":"state"}`, true}} {
		_, err := ParseMessage([]byte(test.message))
		if (err != nil) != test.wantErr {
			t.Fatalf("ParseMessage(%s) = %v", test.message, err)
		}
	}
}

func TestServer_WebSocket(t *testing.T) {
	t.Parallel()

	server, store := newTestServer(t)

	token, err := auth.NewStore(store).RotateBearer()
	if err != nil {
		t.Fatal(err)
	}

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	connection, _, _, err := (ws.Dialer{Header: ws.HandshakeHeaderHTTP(http.Header{"Authorization": []string{"Bearer " + token}})}).Dial(context.Background(), "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/api/v3/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close() //nolint:errcheck // test cleanup

	assertMessage(t, connection, "state", "")

	if wsutil.WriteClientText(connection, []byte(`{"type":"ping","id":"ping-1"}`)) != nil {
		t.Fatal("write ping")
	}

	assertMessage(t, connection, "pong", "ping-1")

}

func TestServer_StateIncludesPublicEntrypoints(t *testing.T) {
	t.Parallel()

	server, store := newTestServer(t)
	token, err := auth.NewStore(store).RotateBearer()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v3/state", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	var body struct {
		State struct {
			Entrypoints []struct {
				ID           string `json:"id"`
				Label        string `json:"label"`
				DevAddr      string `json:"devaddr"`
				Capabilities struct {
					Unlock bool `json:"unlock"`
				} `json:"capabilities"`
			} `json:"entrypoints"`
		} `json:"state"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil {
		t.Fatalf("state response = %s", response.Body.String())
	}
	if len(body.State.Entrypoints) != 1 || body.State.Entrypoints[0].ID != "main" || !body.State.Entrypoints[0].Capabilities.Unlock || body.State.Entrypoints[0].DevAddr != "" {
		t.Fatalf("entrypoints = %#v", body.State.Entrypoints)
	}
}

func TestServer_UnlockEntrypoint(t *testing.T) {
	t.Parallel()

	server, store := newTestServer(t)
	control := &unlockRecorder{}
	server.SetEntrypoints(control)
	token, err := auth.NewStore(store).RotateBearer()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/entrypoints/main/unlock", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || control.entrypoint != "main" {
		t.Fatalf("unlock response = %d, entrypoint = %q", response.Code, control.entrypoint)
	}

	request = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/entrypoints/missing/unlock", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown entrypoint response = %d", response.Code)
	}
}

func TestServer_SystemRebootRespondsBeforeTerminalError(t *testing.T) {
	t.Parallel()

	server, _ := newTestServer(t)
	var logs bytes.Buffer
	server.logger = slog.New(slog.NewTextHandler(&logs, nil))
	runtime := &runtimeRecorder{rebootErr: errors.New("shutdown failed"), rebooted: make(chan struct{})}
	server.SetRuntime(runtime)
	token, err := server.auth.RotateBearer()
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/system/reboot", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("reboot response = %d: %s", response.Code, response.Body.String())
	}
	select {
	case <-runtime.rebooted:
	case <-time.After(time.Second):
		t.Fatal("reboot was not called")
	}
	if strings.Count(logs.String(), "reboot system") != 1 {
		t.Fatalf("reboot logs = %q", logs.String())
	}
}

func TestServer_StateRebootRequiresAvailableRuntime(t *testing.T) {
	t.Parallel()

	server, _ := newTestServer(t)
	server.SetRuntime(&runtimeRecorder{})
	if server.currentPayload().SystemControl.RebootEnabled {
		t.Fatal("reboot should not be enabled without an available adapter")
	}
}

func TestServer_SystemRebootRespectsConfig(t *testing.T) {
	t.Parallel()

	server, store := newTestServer(t)
	server.SetRuntime(&runtimeRecorder{available: true})
	if err := store.Update(func(cfg *config.Config) error {
		cfg.System.RebootEnabled = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	token, err := server.auth.RotateBearer()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/system/reboot", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("reboot response = %d: %s", response.Code, response.Body.String())
	}
}

func TestServer_DiagnosticSourceRequiresBearerAndDirectlyControlsSession(t *testing.T) {
	t.Parallel()

	server, _ := newTestServer(t)
	source := &diagnosticSourceRecorder{}
	server.SetDiagnosticSource(source)
	token, err := server.auth.RotateBearer()
	if err != nil {
		t.Fatal(err)
	}

	unauthorized := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/diagnostics/source/start", nil)
	unauthorizedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized || source.starts != 0 {
		t.Fatalf("unauthorized response = %d, starts = %d", unauthorizedResponse.Code, source.starts)
	}

	for _, path := range []string{"/api/v3/diagnostics/source/start", "/api/v3/diagnostics/source/stop"} {
		request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s response = %d", path, response.Code)
		}
	}
	if source.starts != 1 || source.stops != 1 || !source.startDetached {
		t.Fatalf("source calls = start %d, stop %d, start detached = %t", source.starts, source.stops, source.startDetached)
	}
}

func TestServer_SlowClientWriteFailureDisconnects(t *testing.T) {
	t.Parallel()

	server, _ := newTestServer(t)
	server.clients.add(&client{conn: failingConn{}})
	server.BroadcastState()

	if len(server.clients.all()) != 0 {
		t.Fatal("failed client remained registered")
	}
}

func assertMessage(t *testing.T, connection net.Conn, expectedType, expectedID string) {
	t.Helper()

	data, _, err := wsutil.ReadServerData(connection)
	if err != nil {
		t.Fatal(err)
	}

	var message Message
	if json.Unmarshal(data, &message) != nil || message.Type != expectedType || message.ID != expectedID {
		t.Fatalf("message = %s", data)
	}
}

func newTestServer(t *testing.T) (*Server, *config.Store) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.Create(path, config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"}); err != nil {
		t.Fatal(err)
	}

	store, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	return NewServer(auth.NewStore(store), store, core.NewProjector(), slog.New(slog.DiscardHandler)), store
}

type failingConn struct{}

type unlockRecorder struct {
	entrypoint core.EntrypointID
}

func (r *unlockRecorder) Unlock(_ context.Context, id core.EntrypointID) error {
	r.entrypoint = id
	return nil
}

type diagnosticSourceRecorder struct {
	starts        int
	stops         int
	startDetached bool
}

type runtimeRecorder struct {
	available bool
	rebootErr error
	rebooted  chan struct{}
	once      sync.Once
}

func (r *runtimeRecorder) Reboot(context.Context) error {
	if r.rebooted != nil {
		r.once.Do(func() { close(r.rebooted) })
	}
	return r.rebootErr
}

func (r *runtimeRecorder) RebootAvailable() bool               { return r.available || r.rebooted != nil }
func (*runtimeRecorder) Restart(context.Context, string) error { return nil }
func (*runtimeRecorder) Status(context.Context, string) (system.ServiceStatus, error) {
	return system.ServiceStatus{}, nil
}

func (r *diagnosticSourceRecorder) Start(ctx context.Context) error {
	r.starts++
	r.startDetached = ctx.Done() == nil
	return nil
}

func (r *diagnosticSourceRecorder) Close(context.Context) error {
	r.stops++
	return nil
}

func (failingConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (failingConn) Write([]byte) (int, error)        { return 0, errors.New("write failed") }
func (failingConn) Close() error                     { return nil }
func (failingConn) LocalAddr() net.Addr              { return nil }
func (failingConn) RemoteAddr() net.Addr             { return nil }
func (failingConn) SetDeadline(time.Time) error      { return nil }
func (failingConn) SetReadDeadline(time.Time) error  { return nil }
func (failingConn) SetWriteDeadline(time.Time) error { return nil }

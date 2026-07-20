package api

import (
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"bticino-go-companion/internal/system"
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

	server, _ := newTestServer(t)
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

	claimCode, err := server.auth.InitialClaimCode()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/pair/claim", strings.NewReader(`{"challenge_id":"`+challenge.ChallengeID+`","claim_code":"`+claimCode+`"}`))
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

func TestServer_AuthStatusPublishesSafePairingState(t *testing.T) {
	t.Parallel()

	server, store := newTestServer(t)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v3/auth/status", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	var status struct {
		InstanceID   string `json:"instance_id"`
		PairingState string `json:"pairing_state"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &status) != nil {
		t.Fatalf("auth status response = %d: %s", response.Code, response.Body.String())
	}
	if status.PairingState != string(config.PairingStateClaimable) {
		t.Fatalf("pairing state = %q, want claimable", status.PairingState)
	}
	if status.InstanceID != store.Snapshot().Auth.InstanceID {
		t.Fatalf("instance id = %q, want %q", status.InstanceID, store.Snapshot().Auth.InstanceID)
	}
}

func TestServer_RepairRecovery(t *testing.T) {
	t.Parallel()

	server, _ := newTestServer(t)
	token, err := server.auth.RotateBearer()
	if err != nil {
		t.Fatal(err)
	}

	repair, _, err := server.auth.IssueRepairCode()
	if err != nil {
		t.Fatal(err)
	}
	withoutRepair := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/auth/recover", strings.NewReader(`{}`))
	withoutRepair.Header.Set("Authorization", "Bearer "+token)
	withoutRepairResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(withoutRepairResponse, withoutRepair)
	if withoutRepairResponse.Code != http.StatusUnauthorized {
		t.Fatalf("old bearer recovery status = %d: %s", withoutRepairResponse.Code, withoutRepairResponse.Body.String())
	}

	recoverRequest := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/auth/recover", strings.NewReader(`{"repair_code":"`+repair+`"}`))
	recoverResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(recoverResponse, recoverRequest)
	var recovered struct {
		AccessToken string `json:"access_token"`
	}
	if recoverResponse.Code != http.StatusOK || json.Unmarshal(recoverResponse.Body.Bytes(), &recovered) != nil || recovered.AccessToken == "" {
		t.Fatalf("recover bearer response = %s", recoverResponse.Body.String())
	}
	if server.auth.ValidateBearer(token) {
		t.Fatal("recovery did not replace bearer")
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

	connection, bufferedReader, _, err := (ws.Dialer{Header: ws.HandshakeHeaderHTTP(http.Header{"Authorization": []string{"Bearer " + token}})}).Dial(context.Background(), "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/api/v3/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close() //nolint:errcheck // test cleanup
	reader := io.Reader(connection)
	if bufferedReader != nil {
		reader = bufferedReader
	}
	transport := struct {
		io.Reader
		io.Writer
	}{Reader: reader, Writer: connection}

	assertMessage(t, transport, "state", "")

	if wsutil.WriteClientText(connection, []byte(`{"type":"ping","id":"ping-1"}`)) != nil {
		t.Fatal("write ping")
	}

	assertMessage(t, transport, "pong", "ping-1")

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

func TestServer_WebRTCContract(t *testing.T) {
	server, _ := newTestServer(t)
	control := &webRTCRecorder{answer: "answer-sdp"}
	server.SetWebRTC(control)
	token, err := server.auth.RotateBearer()
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	connection, bufferedReader, _, err := (ws.Dialer{Header: ws.HandshakeHeaderHTTP(http.Header{"Authorization": []string{"Bearer " + token}})}).Dial(context.Background(), "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/api/v3/webrtc/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close() //nolint:errcheck // test cleanup
	reader := io.Reader(connection)
	if bufferedReader != nil {
		reader = bufferedReader
	}
	transport := struct {
		io.Reader
		io.Writer
	}{Reader: reader, Writer: connection}
	write := func(message string) map[string]any {
		t.Helper()
		if err := wsutil.WriteClientText(connection, []byte(message)); err != nil {
			t.Fatal(err)
		}
		data, err := wsutil.ReadServerText(transport)
		if err != nil {
			t.Fatal(err)
		}
		var response map[string]any
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	offer := write(`{"type":"offer","id":"session-1","payload":{"session_id":"session-1","entrypoint_id":"main","origin":"native_camera","offer_sdp":"offer-sdp"}}`)
	if offer["type"] != "answer" || control.sessionID != "session-1" || control.entrypointID != "main" || control.offerSDP != "offer-sdp" {
		t.Fatalf("offer response = %#v, call = %#v", offer, control)
	}
	candidate := write(`{"type":"candidate","id":"session-1","payload":{"session_id":"session-1","candidate":{"candidate":"candidate:1 1 udp 1 192.0.2.1 12345 typ host","sdpMid":"0","sdpMLineIndex":0,"usernameFragment":"ufrag"}}}`)
	if candidate["type"] != "ack" || control.candidateSessionID != "session-1" || control.candidate.Candidate == "" {
		t.Fatalf("candidate response = %#v, call = %#v", candidate, control)
	}
	close := write(`{"type":"close","id":"session-1","payload":{"session_id":"session-1","reason":"test"}}`)
	if close["type"] != "ack" || control.closed != "session-1" {
		t.Fatalf("close response = %#v, call = %#v", close, control)
	}
}

func TestServer_WebRTCLocalCandidatesFollowAnswerInOrder(t *testing.T) {
	server, _ := newTestServer(t)
	server.SetWebRTC(&webRTCRecorder{answer: "answer-sdp", localCandidates: []*media.ICECandidate{{Candidate: "candidate:1"}, nil}})
	token, err := server.auth.RotateBearer()
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	connection, bufferedReader, _, err := (ws.Dialer{Header: ws.HandshakeHeaderHTTP(http.Header{"Authorization": []string{"Bearer " + token}})}).Dial(context.Background(), "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/api/v3/webrtc/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close() //nolint:errcheck // test cleanup
	reader := io.Reader(connection)
	if bufferedReader != nil {
		reader = bufferedReader
	}
	transport := struct {
		io.Reader
		io.Writer
	}{Reader: reader, Writer: connection}
	if err := wsutil.WriteClientText(connection, []byte(`{"type":"offer","id":"session-1","payload":{"session_id":"session-1","entrypoint_id":"main","origin":"native_camera","offer_sdp":"offer-sdp"}}`)); err != nil {
		t.Fatal(err)
	}

	for i, wantType := range []string{"answer", "candidate", "candidate"} {
		data, err := wsutil.ReadServerText(transport)
		if err != nil {
			t.Fatal(err)
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		if message["type"] != wantType {
			t.Fatalf("message %d type = %q, want %q", i, message["type"], wantType)
		}
	}
}

func TestServer_SnapshotLatest(t *testing.T) {
	server, _ := newTestServer(t)
	server.SetSnapshot(snapshotRecorder{image: []byte{0xff, 0xd8, 1, 0xff, 0xd9}})
	token, err := server.auth.RotateBearer()
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v3/entrypoints/main/snapshot/latest.jpg", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/jpeg" || string(response.Body.Bytes()) != string([]byte{0xff, 0xd8, 1, 0xff, 0xd9}) {
		t.Fatalf("snapshot response = %d, headers = %#v, body = %v", response.Code, response.Header(), response.Body.Bytes())
	}

	server.SetSnapshot(snapshotRecorder{err: media.ErrSnapshotNotFound})
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing snapshot response = %d", response.Code)
	}
}

func TestServer_SystemRebootRespondsBeforeTerminalError(t *testing.T) {
	t.Parallel()

	server, _ := newTestServer(t)
	logs := &signalWriter{written: make(chan struct{})}
	server.logger = slog.New(slog.NewTextHandler(logs, nil))
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
	deadline := time.Now().Add(time.Second)
	for strings.Count(logs.String(), "system reboot failed") != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if strings.Count(logs.String(), "system reboot failed") != 1 {
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

func TestServer_SystemServiceRestartAcknowledgesBeforeRestart(t *testing.T) {
	t.Parallel()

	server, store := newTestServer(t)
	runtime := &runtimeRecorder{restarted: make(chan string, 1)}
	server.SetRuntime(runtime)
	if err := store.Update(func(cfg *config.Config) error {
		cfg.System.Services = map[string]config.Service{
			"companion": {Enabled: true, Exposed: true},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	token, err := server.auth.RotateBearer()
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/system/services/companion/restart", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("restart response = %d: %s", response.Code, response.Body.String())
	}
	select {
	case service := <-runtime.restarted:
		if service != "companion" {
			t.Fatalf("restarted service = %q, want companion", service)
		}
	case <-time.After(time.Second):
		t.Fatal("service restart was not called")
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

func TestServer_CloseWebSocketsClosesStateAndWebRTCConnections(t *testing.T) {
	t.Parallel()

	server, _ := newTestServer(t)
	stateConn, statePeer := net.Pipe()
	webrtcConn, webrtcPeer := net.Pipe()
	defer statePeer.Close()  //nolint:errcheck // test cleanup
	defer webrtcPeer.Close() //nolint:errcheck // test cleanup
	server.clients.add(&client{conn: stateConn})
	server.webrtcClients.add(&client{conn: webrtcConn})

	server.CloseWebSockets()

	for _, peer := range []net.Conn{statePeer, webrtcPeer} {
		if _, err := peer.Write([]byte("x")); err == nil {
			t.Fatal("websocket peer remained connected")
		}
	}
}

func assertMessage(t *testing.T, reader io.ReadWriter, expectedType, expectedID string) {
	t.Helper()

	data, _, err := wsutil.ReadServerData(reader)
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

	authStore := auth.NewStore(store)
	if err := store.Update(func(cfg *config.Config) error {
		cfg.WebUI.SessionSecret = "test-session-secret"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := authStore.StartInitialClaim(); err != nil {
		t.Fatal(err)
	}

	return NewServer(authStore, store, core.NewProjector(), slog.New(slog.DiscardHandler)), store
}

type failingConn struct{}

type unlockRecorder struct {
	entrypoint core.EntrypointID
}

type snapshotRecorder struct {
	image []byte
	err   error
}

func (r snapshotRecorder) Latest(string) ([]byte, error) {
	return r.image, r.err
}

type webRTCRecorder struct {
	answer                            string
	sessionID, entrypointID, offerSDP string
	closed                            string
	candidateSessionID                string
	candidate                         media.ICECandidate
	localCandidates                   []*media.ICECandidate
}

func (r *webRTCRecorder) Offer(_ context.Context, sessionID, entrypointID, offerSDP string, onLocalCandidate func(*media.ICECandidate)) (string, error) {
	r.sessionID, r.entrypointID, r.offerSDP = sessionID, entrypointID, offerSDP
	for _, candidate := range r.localCandidates {
		onLocalCandidate(candidate)
	}
	return r.answer, nil
}

func (r *webRTCRecorder) AddICECandidate(sessionID string, candidate media.ICECandidate) error {
	r.candidateSessionID, r.candidate = sessionID, candidate
	return nil
}

func (r *webRTCRecorder) Close(sessionID string) error {
	r.closed = sessionID
	return nil
}

func (r *unlockRecorder) Unlock(_ context.Context, id core.EntrypointID) error {
	r.entrypoint = id
	return nil
}

type runtimeRecorder struct {
	available bool
	rebootErr error
	rebooted  chan struct{}
	restarted chan string
	once      sync.Once
}

type signalWriter struct {
	mu      sync.Mutex
	value   strings.Builder
	written chan struct{}
	once    sync.Once
}

func (w *signalWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.value.Write(data)
	w.once.Do(func() { close(w.written) })
	return n, err
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.value.String()
}

func (r *runtimeRecorder) Reboot(context.Context) error {
	if r.rebooted != nil {
		r.once.Do(func() { close(r.rebooted) })
	}
	return r.rebootErr
}

func (r *runtimeRecorder) RebootAvailable() bool      { return r.available || r.rebooted != nil }
func (*runtimeRecorder) ServiceAvailable(string) bool { return true }
func (r *runtimeRecorder) Restart(_ context.Context, service string) error {
	if r.restarted != nil {
		r.restarted <- service
	}
	return nil
}
func (*runtimeRecorder) Status(context.Context, string) (system.ServiceStatus, error) {
	return system.ServiceStatus{}, nil
}

func (failingConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (failingConn) Write([]byte) (int, error)        { return 0, errors.New("write failed") }
func (failingConn) Close() error                     { return nil }
func (failingConn) LocalAddr() net.Addr              { return nil }
func (failingConn) RemoteAddr() net.Addr             { return nil }
func (failingConn) SetDeadline(time.Time) error      { return nil }
func (failingConn) SetReadDeadline(time.Time) error  { return nil }
func (failingConn) SetWriteDeadline(time.Time) error { return nil }

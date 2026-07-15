package api

import (
	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
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
	}{{`{"type":"ping","id":"ping-1"}`, false}, {`{"type":"command","id":"state-1","action":"state.get"}`, false}, {`{"type":"command","id":"state-1","action":"state.get","payload":[]}`, true}, {`{"type":"state"}`, true}} {
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

	if wsutil.WriteClientText(connection, []byte(`{"type":"command","id":"state-1","action":"state.get"}`)) != nil {
		t.Fatal("write command")
	}

	assertMessage(t, connection, "command_result", "state-1")
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

	return NewServer(auth.NewStore(store), store, core.NewProjector(), nil, slog.New(slog.DiscardHandler)), store
}

type failingConn struct{}

func (failingConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (failingConn) Write([]byte) (int, error)        { return 0, errors.New("write failed") }
func (failingConn) Close() error                     { return nil }
func (failingConn) LocalAddr() net.Addr              { return nil }
func (failingConn) RemoteAddr() net.Addr             { return nil }
func (failingConn) SetDeadline(time.Time) error      { return nil }
func (failingConn) SetReadDeadline(time.Time) error  { return nil }
func (failingConn) SetWriteDeadline(time.Time) error { return nil }

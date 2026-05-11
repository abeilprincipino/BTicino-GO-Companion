package v2

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"bticino-go-companion/internal/auth"
	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/domain/event"
	"bticino-go-companion/internal/security"
	"bticino-go-companion/internal/services/control"
	"bticino-go-companion/internal/services/events"
	"bticino-go-companion/internal/services/runtime"
	"bticino-go-companion/internal/services/state"
	"bticino-go-companion/internal/services/trace"
)

type unlockNoop struct{}

func (unlockNoop) Unlock(context.Context, string) error { return nil }

type streamNoop struct{}

func (streamNoop) StartForEntrypoint(context.Context, string, string) error { return nil }
func (streamNoop) StopForEntrypoint(context.Context, string) error          { return nil }

type callNoop struct {
	answerErr error
	hangupErr error
}

func (c callNoop) Answer(context.Context) error { return c.answerErr }
func (c callNoop) Hangup(context.Context) error { return c.hangupErr }

func newTestRuntimeStatus() *runtime.Status {
	rt := runtime.New(true, true)
	rt.SetSIPReady(true, "")
	rt.SetOpenWebNetReady(true, "")
	rt.SetControlReady(true, "")
	return rt
}

func newClaimedAuth(t *testing.T) (*auth.Store, string) {
	t.Helper()
	store, err := auth.NewStore(filepath.Join(t.TempDir(), "config.json"), "abcd-1234", "C300X", "00:03:50:96:2e:38")
	if err != nil {
		t.Fatalf("new auth store: %v", err)
	}
	ch, err := store.StartChallenge()
	if err != nil {
		t.Fatalf("start challenge: %v", err)
	}
	token, _, err := store.Claim(auth.ClaimRequest{
		ChallengeID: ch.ID,
		Nonce:       ch.Nonce,
		ClaimCode:   "abcd-1234",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return store, token
}

func authReq(method string, path string, token string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func newAuthedRouter(t *testing.T) (*Router, string) {
	t.Helper()
	authStore, token := newClaimedAuth(t)
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, nil)
	r := NewRouter(authStore, security.NewGuard(), p, c, events.New(32), newTestRuntimeStatus(), trace.New(16))
	return r, token
}

func TestRouterStateEndpoint(t *testing.T) {
	r, token := newAuthedRouter(t)
	req := authReq(http.MethodGet, "/api/v2/state", token)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		CallState   string             `json:"call_state"`
		Entrypoints []entrypoint.Model `json:"entrypoints"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.CallState != "idle" {
		t.Fatalf("unexpected call_state: %s", body.CallState)
	}
	if len(body.Entrypoints) != 1 || body.Entrypoints[0].ID != "main" {
		t.Fatalf("unexpected entrypoints payload: %+v", body.Entrypoints)
	}
}

func TestRouterRejectsUnauthorizedProtectedEndpoint(t *testing.T) {
	r, _ := newAuthedRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/state", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRouterPairChallengeAndClaim(t *testing.T) {
	authStore, err := auth.NewStore(filepath.Join(t.TempDir(), "config.json"), "abcd-1234", "C300X", "00:03:50:96:2e:38")
	if err != nil {
		t.Fatalf("new auth store: %v", err)
	}
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, nil)
	r := NewRouter(authStore, security.NewGuard(), p, c, events.New(8), newTestRuntimeStatus(), trace.New(8))

	req := httptest.NewRequest(http.MethodPost, "/api/v2/pair/challenge", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("challenge expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var ch struct {
		ChallengeID string `json:"challenge_id"`
		Nonce       string `json:"nonce"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &ch); err != nil {
		t.Fatalf("decode challenge body: %v", err)
	}
	body := `{"challenge_id":"` + ch.ChallengeID + `","nonce":"` + ch.Nonce + `","claim_code":"abcd-1234"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v2/pair/claim", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("claim expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRouterEventsSSEReplay(t *testing.T) {
	r, token := newAuthedRouter(t)
	r.events.Publish(event.Envelope{ID: 1, Type: event.TypeRingStarted})
	r.events.Publish(event.Envelope{ID: 2, Type: event.TypeStreamStarted})

	ctx, cancel := context.WithCancel(context.Background())
	req := authReq(http.MethodGet, "/api/v2/events?last_event_id=1", token).WithContext(ctx)
	cancel()
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() == "" {
		t.Fatal("expected non-empty replay")
	}
}

func TestRouterCallControlEndpoints(t *testing.T) {
	r, token := newAuthedRouter(t)
	req := authReq(http.MethodPost, "/api/v2/control/call/answer", token)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on answer, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = authReq(http.MethodPost, "/api/v2/control/call/hangup", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on hangup, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRouterOpenWebNetTraceEndpoint(t *testing.T) {
	r, token := newAuthedRouter(t)
	r.trace.Publish(trace.Record{Direction: "rx", Transport: "udp_multicast", Frame: "*8*1#1#4#21*10##", Mapped: true})
	req := authReq(http.MethodGet, "/api/v2/trace/openwebnet", token)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		Records []trace.Record `json:"records"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Records) != 1 || body.Records[0].Frame == "" {
		t.Fatalf("unexpected trace records: %+v", body.Records)
	}
}

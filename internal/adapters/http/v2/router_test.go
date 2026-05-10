package v2

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/domain/event"
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

func TestRouterStateEndpoint(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, nil)
	r := NewRouter(p, c, events.New(32), newTestRuntimeStatus(), trace.New(16))
	req := httptest.NewRequest(http.MethodGet, "/api/v2/state", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
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

func TestRouterCapabilitiesEndpoint(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, nil)
	r := NewRouter(p, c, events.New(32), newTestRuntimeStatus(), trace.New(16))
	req := httptest.NewRequest(http.MethodGet, "/api/v2/capabilities", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		APIVersion   string   `json:"api_version"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.APIVersion != "v2" {
		t.Fatalf("unexpected api_version: %s", body.APIVersion)
	}
	if len(body.Capabilities) != 5 {
		t.Fatalf("unexpected capabilities payload: %+v", body.Capabilities)
	}
}

func TestRouterEntrypointUnlockEndpoint(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, nil)
	r := NewRouter(p, c, events.New(32), newTestRuntimeStatus(), trace.New(16))
	req := httptest.NewRequest(http.MethodPost, "/api/v2/control/entrypoints/main/unlock", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		OK           bool   `json:"ok"`
		EntrypointID string `json:"entrypoint_id"`
		Action       string `json:"action"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body.OK || body.EntrypointID != "main" || body.Action != "unlock" {
		t.Fatalf("unexpected response payload: %+v", body)
	}
}

func TestRouterEventsSSEReplay(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	b := events.New(32)
	b.Publish(event.Envelope{ID: 1, Type: event.TypeRingStarted})
	b.Publish(event.Envelope{ID: 2, Type: event.TypeStreamStarted})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, nil)
	r := NewRouter(p, c, b, newTestRuntimeStatus(), trace.New(16))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/v2/events?last_event_id=1", nil).WithContext(ctx)
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

func TestRouterEntrypointControlNotFoundErrorEnvelope(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, nil)
	r := NewRouter(p, c, events.New(8), newTestRuntimeStatus(), trace.New(16))

	req := httptest.NewRequest(http.MethodPost, "/api/v2/control/entrypoints/unknown/unlock", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		Error struct {
			Code      string `json:"code"`
			Status    int    `json:"status"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code != "entrypoint_not_found" || body.Error.Status != http.StatusNotFound || body.Error.Retryable {
		t.Fatalf("unexpected error envelope: %+v", body.Error)
	}
}

func TestRouterControlUnavailableEnvelope(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	r := NewRouter(p, nil, events.New(8), newTestRuntimeStatus(), trace.New(16))

	req := httptest.NewRequest(http.MethodPost, "/api/v2/control/entrypoints/main/stream/start", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		Error struct {
			Code      string `json:"code"`
			Status    int    `json:"status"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code != "control_unavailable" || body.Error.Status != http.StatusServiceUnavailable || !body.Error.Retryable {
		t.Fatalf("unexpected error envelope: %+v", body.Error)
	}
}

func TestRouterCallControlEndpoints(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, nil)
	r := NewRouter(p, c, events.New(8), newTestRuntimeStatus(), trace.New(16))

	req := httptest.NewRequest(http.MethodPost, "/api/v2/control/call/answer", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on answer, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v2/control/call/hangup", nil)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on hangup, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRouterOpenWebNetTraceEndpoint(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	tb := trace.New(8)
	tb.Publish(trace.Record{Direction: "rx", Transport: "udp_multicast", Frame: "*8*1#1#4#21*10##", Mapped: true})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, nil)
	r := NewRouter(p, c, events.New(8), newTestRuntimeStatus(), tb)

	req := httptest.NewRequest(http.MethodGet, "/api/v2/trace/openwebnet", nil)
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

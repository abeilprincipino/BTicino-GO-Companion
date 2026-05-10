package v2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/domain/event"
	"bticino-go-companion/internal/services/control"
	"bticino-go-companion/internal/services/events"
	"bticino-go-companion/internal/services/state"
)

type unlockNoop struct{}

func (unlockNoop) Unlock(context.Context, string) error { return nil }

type streamNoop struct{}

func (streamNoop) StreamStart(context.Context, string) error { return nil }
func (streamNoop) StreamStop(context.Context) error          { return nil }

func TestRouterStateEndpoint(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, nil)
	r := NewRouter(p, c, events.New(32))
	req := httptest.NewRequest(http.MethodGet, "/api/v2/state", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestRouterCapabilitiesEndpoint(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, nil)
	r := NewRouter(p, c, events.New(32))
	req := httptest.NewRequest(http.MethodGet, "/api/v2/capabilities", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "entrypoints_v2") {
		t.Fatalf("unexpected capabilities payload: %s", rr.Body.String())
	}
}

func TestRouterEntrypointUnlockEndpoint(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, nil)
	r := NewRouter(p, c, events.New(32))
	req := httptest.NewRequest(http.MethodPost, "/api/v2/control/entrypoints/main/unlock", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestRouterEventsSSEReplay(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	b := events.New(32)
	b.Publish(event.Envelope{ID: 1, Type: "ring.started"})
	b.Publish(event.Envelope{ID: 2, Type: "stream.started"})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, nil)
	r := NewRouter(p, c, b)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/v2/events?last_event_id=1", nil).WithContext(ctx)
	cancel()
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "\"id\":2") {
		t.Fatalf("expected replay with id 2, got %s", rr.Body.String())
	}
}

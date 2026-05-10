package v2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/services/control"
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
	r := NewRouter(p, c)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/state", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestRouterEntrypointUnlockEndpoint(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	c := control.New(p.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, nil)
	r := NewRouter(p, c)
	req := httptest.NewRequest(http.MethodPost, "/api/v2/control/entrypoints/main/unlock", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}

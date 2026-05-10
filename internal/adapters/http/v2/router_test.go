package v2

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/services/state"
)

func TestRouterStateEndpoint(t *testing.T) {
	p := state.NewProjector([]entrypoint.Model{{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	r := NewRouter(p)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/state", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

package v2

import (
	"encoding/json"
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
	"bticino-go-companion/internal/services/snapshot"
	"bticino-go-companion/internal/services/state"
	"bticino-go-companion/internal/services/trace"
)

func TestEntrypointSnapshotLatestAndUnavailableCapture(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Entrypoints = []entrypoint.Model{
		{ID: "main", Label: "Main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true},
	}
	authStore, token := newClaimedAuth(t)
	projector := state.NewProjector(cfg.Entrypoints)
	ctrl := control.New(projector.Snapshot().Entrypoints, streamNoop{}, unlockNoop{}, callNoop{}, audioNoop{}, voicemailNoop{}, nil)
	snapSvc := snapshot.New(cfg, nil, nil, nil)
	r := NewRouter(cfg, authStore, projector, ctrl, events.New(16), newTestRuntimeStatus(), trace.New(16), nil, nil, nil, snapSvc)

	snapshotDir := filepath.Join(cfg.DataDir, "media", "snapshots")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshot dir: %v", err)
	}
	want := []byte{0xFF, 0xD8, 0xFF, 0xD9}
	if err := os.WriteFile(filepath.Join(snapshotDir, "main.jpg"), want, 0o644); err != nil {
		t.Fatalf("write snapshot file: %v", err)
	}

	req := authReq(http.MethodGet, "/api/v2/entrypoints/main/snapshot/latest.jpg", token)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("latest snapshot expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.HasPrefix(rr.Header().Get("Content-Type"), "image/jpeg") {
		t.Fatalf("latest snapshot content-type expected image/jpeg, got %q", rr.Header().Get("Content-Type"))
	}
	if got := rr.Body.Bytes(); len(got) != len(want) || got[0] != want[0] || got[len(got)-1] != want[len(want)-1] {
		t.Fatalf("latest snapshot payload mismatch got=%v want=%v", got, want)
	}

	req = authReq(http.MethodPost, "/api/v2/control/entrypoints/main/snapshot", token)
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("snapshot capture expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	errPayload, _ := body["error"].(map[string]any)
	if code, _ := errPayload["code"].(string); code != "snapshot_unavailable" {
		t.Fatalf("unexpected error code: %v", errPayload)
	}
}


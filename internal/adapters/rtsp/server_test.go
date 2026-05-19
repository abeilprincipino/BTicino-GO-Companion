package rtspadapter

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/domain/entrypoint"
)

type lifecycleRecorder struct {
	joinCalls  int
	leaveCalls int
	touchCalls int
	lastJoinID string
	lastDev    string
}

func (r *lifecycleRecorder) ReaderJoin(_ context.Context, sessionID string, entrypointID string, devAddr string) error {
	r.joinCalls++
	r.lastJoinID = entrypointID
	r.lastDev = devAddr
	return nil
}

func (r *lifecycleRecorder) ReaderLeave(_ context.Context, _ string) error {
	r.leaveCalls++
	return nil
}

func (r *lifecycleRecorder) ReaderTouch(_ string) {
	r.touchCalls++
}

func TestServerPathAndLifecycleHooks(t *testing.T) {
	cfg := config.Default()
	cfg.Entrypoints = []entrypoint.Model{
		{ID: "gate1", DevAddr: "20", HasStream: true},
		{ID: "gate2", DevAddr: "21", HasStream: true},
	}

	rec := &lifecycleRecorder{}
	s := NewServer(cfg, log.New(io.Discard, "", 0), rec)

	if !s.isKnownPath("/doorbell-gate1") || !s.isKnownPath("/doorbell-gate2") || s.isKnownPath("/doorbell") || s.isKnownPath("/unknown") {
		t.Fatal("unexpected path match behavior")
	}

	session := &gortsplib.ServerSession{}
	playResp, err := s.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{
		Path:    "/doorbell-gate2",
		Session: session,
	})
	if err != nil {
		t.Fatalf("on play returned error: %v", err)
	}
	if playResp == nil || playResp.StatusCode != base.StatusOK {
		t.Fatalf("unexpected play response: %+v", playResp)
	}
	if rec.joinCalls != 1 || rec.lastJoinID != "gate2" || rec.lastDev != "21" {
		t.Fatalf("unexpected lifecycle join payload: %+v", rec)
	}

	pauseResp, err := s.OnPause(&gortsplib.ServerHandlerOnPauseCtx{
		Session: session,
	})
	if err != nil {
		t.Fatalf("on pause returned error: %v", err)
	}
	if pauseResp == nil || pauseResp.StatusCode != base.StatusOK {
		t.Fatalf("unexpected pause response: %+v", pauseResp)
	}
	if rec.leaveCalls != 1 {
		t.Fatalf("expected one leave callback, got %d", rec.leaveCalls)
	}
}

func TestServerDescribeUnknownPath(t *testing.T) {
	cfg := config.Default()
	rec := &lifecycleRecorder{}
	s := NewServer(cfg, log.New(io.Discard, "", 0), rec)

	resp, _, err := s.OnDescribe(&gortsplib.ServerHandlerOnDescribeCtx{Path: "/unknown"})
	if err != nil {
		t.Fatalf("on describe returned error: %v", err)
	}
	if resp == nil || resp.StatusCode != base.StatusNotFound {
		t.Fatalf("unexpected describe response: %+v", resp)
	}
}

func TestServerHelperFunctions(t *testing.T) {
	cfg := config.Default()
	cfg.Entrypoints = []entrypoint.Model{
		{ID: "main", DevAddr: "20", HasStream: true},
	}
	s := NewServer(cfg, log.New(io.Discard, "", 0), &lifecycleRecorder{})

	if id, dev, ok := s.resolveEntrypoint("/doorbell-main"); !ok || id != "main" || dev != "20" {
		t.Fatalf("unexpected resolveEntrypoint result: id=%q dev=%q ok=%v", id, dev, ok)
	}
	if _, _, ok := s.resolveEntrypoint("/missing"); ok {
		t.Fatal("expected unresolved entrypoint for missing path")
	}

	if !isExpectedPayloadType(description.MediaTypeVideo, 96) {
		t.Fatal("expected video payload type 96 to match")
	}
	if isExpectedPayloadType(description.MediaTypeVideo, 110) {
		t.Fatal("did not expect video payload type 110 to match")
	}
	if !isExpectedPayloadType(description.MediaTypeAudio, 110) {
		t.Fatal("expected audio payload type 110 to match")
	}
	if isExpectedPayloadType(description.MediaTypeAudio, 96) {
		t.Fatal("did not expect audio payload type 96 to match")
	}

	got := sortedRoutePaths(map[string]entrypoint.StreamRoute{
		"doorbell-b": {},
		"doorbell-a": {},
	})
	if len(got) != 2 || got[0] != "doorbell-a" || got[1] != "doorbell-b" {
		t.Fatalf("unexpected sorted paths: %+v", got)
	}

	s.touchReader(nil)
	s.removeReader(nil)
	s.writeIngestPacket(description.MediaTypeVideo, nil)

	if id := sessionID(&gortsplib.ServerSession{}); id == "" {
		t.Fatal("expected non-empty session id")
	}
}

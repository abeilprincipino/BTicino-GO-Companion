package rtspadapter

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"

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
	cfg.MediaRTSPPathMain = "doorbell"
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

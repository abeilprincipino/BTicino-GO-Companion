package media

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"testing"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/pion/rtp"
)

func TestRTSPServerUsesConfiguredEntrypointRouteAndDevAddr(t *testing.T) {
	t.Parallel()

	var started config.Entrypoint
	source := &fakeRTSPSource{}
	server := testRTSPServer(t, []config.Entrypoint{
		{ID: "gate1", DevAddr: "20", Capabilities: config.Capabilities{Stream: true}},
		{ID: "gate2", DevAddr: "21", Capabilities: config.Capabilities{Stream: true}},
	}, func(entrypoint config.Entrypoint, packet func(*rtp.Packet)) (RTSPSource, func(), error) {
		if packet == nil {
			t.Fatal("source packet callback is nil")
		}
		started = entrypoint
		return source, nil, nil
	})

	reader := &gortsplib.ServerSession{}
	response, err := server.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{Path: "doorbell-gate2", Session: reader})
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("play = response %#v, error %v", response, err)
	}
	if started.ID != "gate2" || started.DevAddr != "21" {
		t.Fatalf("started entrypoint = %#v, want gate2 / 21", started)
	}

	server.OnSessionClose(&gortsplib.ServerHandlerOnSessionCloseCtx{Session: reader})
	if source.closes != 1 {
		t.Fatalf("source closes = %d, want 1", source.closes)
	}
}

func TestRTSPServerRejectsDifferentEntrypointWhileSourceActive(t *testing.T) {
	t.Parallel()

	source := &fakeRTSPSource{}
	server := testRTSPServer(t, []config.Entrypoint{
		{ID: "gate1", DevAddr: "20", Capabilities: config.Capabilities{Stream: true}},
		{ID: "gate2", DevAddr: "21", Capabilities: config.Capabilities{Stream: true}},
	}, func(config.Entrypoint, func(*rtp.Packet)) (RTSPSource, func(), error) {
		return source, nil, nil
	})

	first := &gortsplib.ServerSession{}
	if response, err := server.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{Path: "doorbell-gate1", Session: first}); err != nil || response.StatusCode != 200 {
		t.Fatalf("first play = response %#v, error %v", response, err)
	}
	response, err := server.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{Path: "doorbell-gate2", Session: &gortsplib.ServerSession{}})
	if response.StatusCode != 400 || !errors.Is(err, ErrEntrypointSwitchBlocked) {
		t.Fatalf("second play = response %#v, error %v", response, err)
	}
}

func testRTSPServer(t *testing.T, entrypoints []config.Entrypoint, factory RTSPSourceFactory) *RTSPServer {
	t.Helper()
	server, err := NewRTSPServer(nil, "", entrypoints, factory)
	if err != nil {
		t.Fatal(err)
	}
	server.ctx = context.Background()
	return server
}

type fakeRTSPSource struct {
	starts int
	closes int
}

func (s *fakeRTSPSource) Start(context.Context) error { s.starts++; return nil }
func (s *fakeRTSPSource) Close(context.Context) error { s.closes++; return nil }

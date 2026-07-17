package media

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"testing"

	"github.com/bluenviron/gortsplib/v5"
)

func TestRTSPServerUsesConfiguredEntrypointRouteAndDevAddr(t *testing.T) {
	t.Parallel()

	var started config.Entrypoint
	source := &fakeManagedSource{}
	server := testRTSPServer(t, []config.Entrypoint{
		{ID: "gate1", DevAddr: "20", Capabilities: config.Capabilities{Stream: true}},
		{ID: "gate2", DevAddr: "21", Capabilities: config.Capabilities{Stream: true}},
	}, func(entrypoint config.Entrypoint, events SourceEvents) (ManagedSource, func(), error) {
		if events.VideoRTP == nil || events.AudioRTP == nil || events.RemoteBYE == nil {
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

	source := &fakeManagedSource{}
	server := testRTSPServer(t, []config.Entrypoint{
		{ID: "gate1", DevAddr: "20", Capabilities: config.Capabilities{Stream: true}},
		{ID: "gate2", DevAddr: "21", Capabilities: config.Capabilities{Stream: true}},
	}, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	first := &gortsplib.ServerSession{}
	if response, err := server.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{Path: "doorbell-gate1", Session: first}); err != nil || response.StatusCode != 200 {
		t.Fatalf("first play = response %#v, error %v", response, err)
	}
	response, err := server.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{Path: "doorbell-gate2", Session: &gortsplib.ServerSession{}})
	if response.StatusCode != 400 || !errors.Is(err, ErrStreamBusy) {
		t.Fatalf("second play = response %#v, error %v", response, err)
	}
}

func TestRTSPServerRejectsExternalStream(t *testing.T) {
	t.Parallel()
	server := testRTSPServer(t, []config.Entrypoint{{ID: "gate1", DevAddr: "20", Capabilities: config.Capabilities{Stream: true}}}, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return &fakeManagedSource{}, nil, nil
	})
	server.ObserveControlTrack(true)
	response, err := server.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{Path: "doorbell-gate1", Session: &gortsplib.ServerSession{}})
	if response.StatusCode != 400 || !errors.Is(err, ErrExternalStream) {
		t.Fatalf("play while external stream active = response %#v, error %v", response, err)
	}
}

func testRTSPServer(t *testing.T, entrypoints []config.Entrypoint, factory ManagedSourceFactory) *RTSPServer {
	t.Helper()
	server, err := NewRTSPServer(nil, "", entrypoints, factory)
	if err != nil {
		t.Fatal(err)
	}
	server.ctx = context.Background()
	return server
}

type fakeManagedSource struct {
	starts int
	closes int
}

func (s *fakeManagedSource) Start(context.Context) error { s.starts++; return nil }
func (s *fakeManagedSource) Close(context.Context) error { s.closes++; return nil }

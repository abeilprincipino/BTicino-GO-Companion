package media

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

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
		if events.VideoRTP == nil || events.AudioRTP == nil || events.RemoteBYE == nil || events.Failed == nil {
			t.Fatal("source packet callback is nil")
		}

		started = entrypoint

		return source, nil, nil
	})

	reader := &gortsplib.ServerSession{}

	response, err := server.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{Path: "doorbell-gate2", Session: reader})
	if err != nil || response.StatusCode != http.StatusOK {
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
	if response, err := server.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{Path: "doorbell-gate1", Session: first}); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("first play = response %#v, error %v", response, err)
	}

	response, err := server.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{Path: "doorbell-gate2", Session: &gortsplib.ServerSession{}})
	if response.StatusCode != http.StatusBadRequest || !errors.Is(err, ErrStreamBusy) {
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
	if response.StatusCode != http.StatusBadRequest || !errors.Is(err, ErrExternalStream) {
		t.Fatalf("play while external stream active = response %#v, error %v", response, err)
	}
}

func TestRTSPServerCloseDuringPlayStartupReleasesSourceOutsideMutex(t *testing.T) {
	source := &blockingManagedSource{started: make(chan struct{}), closed: make(chan struct{})}

	var server *RTSPServer

	server = testRTSPServer(t, []config.Entrypoint{{ID: "gate1", DevAddr: "20", Capabilities: config.Capabilities{Stream: true}}}, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		source.server = server
		return source, nil, nil
	})

	reader := &gortsplib.ServerSession{}
	playResult := make(chan error, 1)

	go func() {
		_, err := server.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{Path: "doorbell-gate1", Session: reader})
		playResult <- err
	}()

	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("source did not start")
	}

	server.OnSessionClose(&gortsplib.ServerHandlerOnSessionCloseCtx{Session: reader})

	if err := <-playResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("play error = %v, want context cancellation", err)
	}

	select {
	case <-source.closed:
	case <-time.After(time.Second):
		t.Fatal("source was not closed")
	}

	if source.closedUnderLock {
		t.Fatal("source was released while RTSP mutex was held")
	}

	server.mu.Lock()
	readers, starting := len(server.readers), len(server.starting)
	server.mu.Unlock()

	if readers != 0 || starting != 0 {
		t.Fatalf("readers=%d starting=%d, want none", readers, starting)
	}

	if snapshot := server.StreamSnapshot(); snapshot.Owner != StreamOwnerIdle {
		t.Fatalf("stream owner = %q, want idle", snapshot.Owner)
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

type blockingManagedSource struct {
	server          *RTSPServer
	started         chan struct{}
	closed          chan struct{}
	closedUnderLock bool
}

func (s *blockingManagedSource) Start(ctx context.Context) error {
	close(s.started)
	<-ctx.Done()

	return nil
}

func (s *blockingManagedSource) Close(context.Context) error {
	if !s.server.mu.TryLock() {
		s.closedUnderLock = true
	} else {
		s.server.mu.Unlock()
	}

	close(s.closed)

	return nil
}

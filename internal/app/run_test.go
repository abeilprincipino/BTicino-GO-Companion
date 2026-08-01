package app

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestOpenConfigCreatesThenReusesConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	detectCalls := 0
	detect := func() (config.Metadata, error) {
		detectCalls++
		return config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"}, nil
	}

	store, created, err := openConfig(path, detect)
	if err != nil {
		t.Fatalf("create config: %v", err)
	}

	if !created {
		t.Fatal("created = false, want true")
	}

	if store.Snapshot().Companion.DeviceID != "c300x-001122334455" {
		t.Fatalf("device id = %q", store.Snapshot().Companion.DeviceID)
	}

	store, created, err = openConfig(path, detect)
	if err != nil {
		t.Fatalf("reuse config: %v", err)
	}

	if created {
		t.Fatal("created = true, want false")
	}

	if detectCalls != 2 {
		t.Fatalf("metadata detection calls = %d, want 2", detectCalls)
	}

	if store.Snapshot().Companion.DeviceID != "c300x-001122334455" {
		t.Fatalf("reopened device id = %q", store.Snapshot().Companion.DeviceID)
	}
}

func TestOpenConfigReturnsMetadataFailure(t *testing.T) {
	t.Parallel()

	_, _, err := openConfig(filepath.Join(t.TempDir(), "config.yaml"), func() (config.Metadata, error) {
		return config.Metadata{}, errors.New("metadata unavailable")
	})
	if err == nil {
		t.Fatal("open config succeeded")
	}
}

func TestResolveInboundEntrypointPrefersPhysicalRing(t *testing.T) {
	t.Parallel()

	entrypoints := []config.Entrypoint{
		{ID: "main", DevAddr: "20"},
		{ID: "side", DevAddr: "21"},
	}

	projector := core.NewProjector()
	if _, err := projector.Apply(core.RingStarted{EntrypointID: "side"}); err != nil {
		t.Fatal(err)
	}

	resolve := newInboundEntrypointResolver(func() []config.Entrypoint { return entrypoints }, projector)

	id, devAddr := resolve()
	if id != "side" || devAddr != "21" {
		t.Fatalf("resolve() = %q/%q, want side/21", id, devAddr)
	}
}

func TestResolveInboundEntrypointFallsBackToSoleEntrypoint(t *testing.T) {
	t.Parallel()

	entrypoints := []config.Entrypoint{{ID: "main", DevAddr: "20"}}
	resolve := newInboundEntrypointResolver(func() []config.Entrypoint { return entrypoints }, core.NewProjector())

	id, devAddr := resolve()
	if id != "main" || devAddr != "20" {
		t.Fatalf("resolve() = %q/%q, want main/20", id, devAddr)
	}
}

func TestResolveInboundEntrypointRefusesAmbiguity(t *testing.T) {
	t.Parallel()

	entrypoints := []config.Entrypoint{{ID: "main", DevAddr: "20"}, {ID: "side", DevAddr: "21"}}
	resolve := newInboundEntrypointResolver(func() []config.Entrypoint { return entrypoints }, core.NewProjector())

	if id, _ := resolve(); id != "" {
		t.Fatalf("resolve() = %q, want empty when the call cannot be attributed", id)
	}
}

func TestServeRunsAPIAndWebUI(t *testing.T) {
	t.Parallel()

	apiListener := testListener(t)
	webUIListener := testListener(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- serve(
			ctx,
			slog.New(slog.DiscardHandler),
			apiListener,
			&http.Server{Handler: http.HandlerFunc(writeOK)},
			webUIListener,
			&http.Server{Handler: http.HandlerFunc(writeOK)},
		)
	}()

	for _, listener := range []net.Listener{apiListener, webUIListener} {
		response, err := http.Get("http://" + listener.Addr().String())
		if err != nil {
			t.Fatalf("request %s: %v", listener.Addr(), err)
		}

		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status %s = %d, want 200", listener.Addr(), response.StatusCode)
		}
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("servers did not shut down")
	}
}

func TestBridgeSourceWritesBackchannelToAudioBridge(t *testing.T) {
	pipeline := &appAudioPipeline{
		opusOut:  make(chan *rtp.Packet),
		speexOut: make(chan *rtp.Packet),
		errors:   make(chan error),
	}

	bridge := media.NewAudioBridge(appGStreamerAudio{pipeline: pipeline}, func(*rtp.Packet) {}, nil, slog.New(slog.DiscardHandler), nil)
	if err := bridge.Start(context.Background()); err != nil {
		t.Fatalf("start bridge: %v", err)
	}

	t.Cleanup(func() {
		if err := bridge.Stop(); err != nil {
			t.Errorf("stop bridge: %v", err)
		}
	})

	packet := &rtp.Packet{Header: rtp.Header{PayloadType: 112}}
	if err := (&bridgeSource{bridge: bridge}).WriteBackchannelRTP(packet); err != nil {
		t.Fatalf("write backchannel RTP: %v", err)
	}

	if pipeline.backchannelPacket != packet {
		t.Fatal("audio bridge did not receive backchannel packet")
	}
}

type appGStreamerAudio struct {
	pipeline media.AudioPipeline
}

func (g appGStreamerAudio) StartAudioBridge(context.Context) (media.AudioPipeline, error) {
	return g.pipeline, nil
}

type appAudioPipeline struct {
	backchannelPacket *rtp.Packet
	opusOut           chan *rtp.Packet
	speexOut          chan *rtp.Packet
	errors            chan error
}

func (*appAudioPipeline) WriteIntercomSpeex(*rtp.Packet) error { return nil }

func (p *appAudioPipeline) WriteBackchannelOpus(packet *rtp.Packet) error {
	p.backchannelPacket = packet
	return nil
}

func (p *appAudioPipeline) ReadOpusOut() <-chan *rtp.Packet  { return p.opusOut }
func (p *appAudioPipeline) ReadSpeexOut() <-chan *rtp.Packet { return p.speexOut }
func (p *appAudioPipeline) Errors() <-chan error             { return p.errors }
func (*appAudioPipeline) Close() error                       { return nil }

func testListener(t *testing.T) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	return listener
}

func writeOK(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

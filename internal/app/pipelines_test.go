package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/media"
	"bticino-go-companion/internal/signaling"
)

var errDialRefused = errors.New("dial refused")

// recordingGStreamerAudio counts the audio bridges started, which is how a test
// sees the GStreamer processes come up.
type recordingGStreamerAudio struct {
	mu     sync.Mutex
	starts int
	closes int
}

func (g *recordingGStreamerAudio) StartAudioBridge(context.Context) (media.AudioPipeline, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.starts++

	return &closeCountingPipeline{
		appAudioPipeline: &appAudioPipeline{opusOut: make(chan *rtp.Packet), speexOut: make(chan *rtp.Packet), errors: make(chan error)},
		owner:            g,
	}, nil
}

func (g *recordingGStreamerAudio) counts() (starts, closes int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.starts, g.closes
}

type closeCountingPipeline struct {
	*appAudioPipeline

	owner *recordingGStreamerAudio
}

func (p *closeCountingPipeline) Close() error {
	p.owner.mu.Lock()
	defer p.owner.mu.Unlock()

	p.owner.closes++

	return nil
}

// ringingStreamDialer holds the INVITE until the test ends it, and rings when told.
type ringingStreamDialer struct {
	mu      sync.Mutex
	ringing func()
	dialed  chan struct{}
	result  chan error
}

func newRingingStreamDialer() *ringingStreamDialer {
	return &ringingStreamDialer{dialed: make(chan struct{}), result: make(chan error, 1)}
}

func (d *ringingStreamDialer) StartStream(ctx context.Context, _, _ string) (signaling.OutgoingDialog, error) {
	close(d.dialed)

	select {
	case err := <-d.result:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (*ringingStreamDialer) SetRemoteDialogEnded(func()) {}

func (d *ringingStreamDialer) SetStreamRinging(callback func()) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.ringing = callback
}

func (d *ringingStreamDialer) ring() {
	d.mu.Lock()
	callback := d.ringing
	d.mu.Unlock()

	callback()
}

func newTestBridgeSource(t *testing.T, dialer *ringingStreamDialer, gstreamer media.GStreamerAudio) media.ManagedSource {
	t.Helper()

	logger := slog.New(slog.DiscardHandler)
	cfg := config.Config{Companion: config.Companion{Model: "C300X"}}
	entrypoint := config.Entrypoint{ID: "main", DevAddr: "20"}
	calls := signaling.NewManager("127.0.0.1", dialer, nil, nil)
	events := media.SourceEvents{AudioRTP: func(*rtp.Packet) {}}

	source, cleanup, err := newBridgeSource(cfg, logger, dialer, calls, gstreamer, entrypoint, events, nil, newRingVideoWatch(logger))
	if err != nil {
		t.Fatalf("newBridgeSource() error = %v", err)
	}

	t.Cleanup(cleanup)

	return source
}

func waitForDial(t *testing.T, dialer *ringingStreamDialer) {
	t.Helper()

	select {
	case <-dialer.dialed:
	case <-time.After(5 * time.Second):
		t.Fatal("the INVITE never went out")
	}
}

func waitForStart(t *testing.T, started <-chan error) error {
	t.Helper()

	select {
	case err := <-started:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Start() never returned")
	}

	return nil
}

func TestBridgeSourceStartsThePipelinesWhenTheIntercomRings(t *testing.T) {
	gstreamer := &recordingGStreamerAudio{}
	dialer := newRingingStreamDialer()
	source := newTestBridgeSource(t, dialer, gstreamer)
	started := make(chan error, 1)

	go func() { started <- source.Start(context.Background()) }()

	waitForDial(t, dialer)

	if starts, _ := gstreamer.counts(); starts != 0 {
		t.Fatalf("audio bridges started before the intercom rang = %d, want 0: they would slow the firmware down while it takes the call", starts)
	}

	dialer.ring()
	dialer.ring()

	if starts, _ := gstreamer.counts(); starts != 1 {
		t.Fatalf("audio bridges started at the ring = %d, want 1", starts)
	}

	// The call failing after the ring takes down what the ring started.
	dialer.result <- errDialRefused

	if err := waitForStart(t, started); !errors.Is(err, errDialRefused) {
		t.Fatalf("Start() error = %v, want %v", err, errDialRefused)
	}

	if _, closes := gstreamer.counts(); closes != 1 {
		t.Fatalf("audio bridges closed after the failed call = %d, want 1", closes)
	}
}

func TestBridgeSourceStartsNoPipelinesForADialRefusedBeforeTheRing(t *testing.T) {
	gstreamer := &recordingGStreamerAudio{}
	dialer := newRingingStreamDialer()
	source := newTestBridgeSource(t, dialer, gstreamer)

	dialer.result <- errDialRefused

	if err := source.Start(context.Background()); !errors.Is(err, errDialRefused) {
		t.Fatalf("Start() error = %v, want %v", err, errDialRefused)
	}

	if starts, _ := gstreamer.counts(); starts != 0 {
		t.Fatalf("audio bridges started for a refused dial = %d, want 0", starts)
	}

	// A ring that was on its way when the stream failed changes nothing.
	dialer.ring()

	if starts, _ := gstreamer.counts(); starts != 0 {
		t.Fatalf("audio bridges started by a ring after the failure = %d, want 0", starts)
	}
}

func TestStreamPipelinesRunOnceWithTheContextOfTheStream(t *testing.T) {
	type streamKey struct{}

	lifecycle := context.WithValue(context.Background(), streamKey{}, "stream")
	failure := errors.New("no gstreamer")
	runs := 0

	var got context.Context

	pipelines := newStreamPipelines(func(ctx context.Context) error {
		runs++
		got = ctx

		return failure
	})
	pipelines.arm(lifecycle)

	for range 2 {
		if err := pipelines.start(); !errors.Is(err, failure) {
			t.Fatalf("start() error = %v, want %v", err, failure)
		}
	}

	if runs != 1 {
		t.Fatalf("runs = %d, want 1", runs)
	}

	if got != lifecycle {
		t.Fatal("the pipelines did not get the context of the stream, which is what ends them")
	}
}

func TestStreamPipelinesWaitForTheStreamToStart(t *testing.T) {
	runs := 0
	pipelines := newStreamPipelines(func(context.Context) error {
		runs++

		return nil
	})

	if err := pipelines.start(); err != nil {
		t.Fatalf("start() before arm() error = %v", err)
	}

	if runs != 0 {
		t.Fatalf("runs before the stream started = %d, want 0", runs)
	}

	pipelines.arm(context.Background())

	if err := pipelines.start(); err != nil {
		t.Fatalf("start() error = %v", err)
	}

	if runs != 1 {
		t.Fatalf("runs = %d, want 1", runs)
	}
}

func TestStreamPipelinesDoNotRunOnceClosed(t *testing.T) {
	runs := 0
	pipelines := newStreamPipelines(func(context.Context) error {
		runs++

		return nil
	})

	pipelines.arm(context.Background())
	pipelines.close()

	if err := pipelines.start(); !errors.Is(err, errStreamPipelinesClosed) {
		t.Fatalf("start() after close() error = %v, want %v", err, errStreamPipelinesClosed)
	}

	if runs != 0 {
		t.Fatalf("runs after close() = %d, want 0", runs)
	}
}

// recordingAV notes the order of what happens to it.
type recordingAV struct {
	media.SourceAV

	steps *[]string
}

func (a recordingAV) Start(context.Context, bool, media.AVPorts, media.FlowProbe, media.FlowProbe) error {
	*a.steps = append(*a.steps, "av")

	return nil
}

func (a recordingAV) StartVideo(context.Context, bool, int, media.FlowProbe) error {
	*a.steps = append(*a.steps, "av")

	return nil
}

func (a recordingAV) StartAudio(context.Context, int, media.FlowProbe) error {
	*a.steps = append(*a.steps, "audio")

	return nil
}

// The 180 is not guaranteed, and a ringing preview never sees one: the last moment
// for the pipelines is right before the intercom is asked for media.
func TestPipelineAVRunsThePipelinesBeforeTheRequest(t *testing.T) {
	requests := map[string]func(media.SourceAV) error{
		"Start":      func(av media.SourceAV) error { return av.Start(context.Background(), false, media.AVPorts{}, nil, nil) },
		"StartVideo": func(av media.SourceAV) error { return av.StartVideo(context.Background(), false, 0, nil) },
	}

	for name, request := range requests {
		t.Run(name, func(t *testing.T) {
			var steps []string

			pipelines := newStreamPipelines(func(context.Context) error {
				steps = append(steps, "pipelines")

				return nil
			})
			pipelines.arm(context.Background())

			if err := request(pipelineAV{SourceAV: recordingAV{steps: &steps}, pipelines: pipelines}); err != nil {
				t.Fatalf("request error = %v", err)
			}

			if len(steps) != 2 || steps[0] != "pipelines" || steps[1] != "av" {
				t.Fatalf("steps = %v, want the pipelines before the AV request", steps)
			}
		})
	}
}

func TestPipelineAVDoesNotAskForMediaWithoutPipelines(t *testing.T) {
	failure := errors.New("no gstreamer")

	var steps []string

	pipelines := newStreamPipelines(func(context.Context) error { return failure })
	pipelines.arm(context.Background())

	av := pipelineAV{SourceAV: recordingAV{steps: &steps}, pipelines: pipelines}

	if err := av.Start(context.Background(), false, media.AVPorts{}, nil, nil); !errors.Is(err, failure) {
		t.Fatalf("Start() error = %v, want %v", err, failure)
	}

	if err := av.StartVideo(context.Background(), false, 0, nil); !errors.Is(err, failure) {
		t.Fatalf("StartVideo() error = %v, want %v", err, failure)
	}

	if len(steps) != 0 {
		t.Fatalf("steps = %v, want no AV request", steps)
	}
}

// By the answer of a ringing preview the pipelines are long up.
func TestPipelineAVLeavesTheAudioRequestAlone(t *testing.T) {
	var steps []string

	runs := 0
	pipelines := newStreamPipelines(func(context.Context) error {
		runs++

		return nil
	})
	pipelines.arm(context.Background())

	if err := (pipelineAV{SourceAV: recordingAV{steps: &steps}, pipelines: pipelines}).StartAudio(context.Background(), 0, nil); err != nil {
		t.Fatalf("StartAudio() error = %v", err)
	}

	if runs != 0 || len(steps) != 1 || steps[0] != "audio" {
		t.Fatalf("runs = %d, steps = %v, want only the audio request", runs, steps)
	}
}

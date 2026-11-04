package app

import (
	"context"
	"errors"
	"sync"

	"bticino-go-companion/internal/media"
)

var errStreamPipelinesClosed = errors.New("stream pipelines closed")

// streamPipelines starts the GStreamer processes of one stream, the audio bridge
// and the snapshot decoder, once the intercom has taken the call.
//
// The intercom has one core, and starting them takes it for about 0.9 s. Started
// before the INVITE they held the firmware back by as much: on a C100X the 180
// came at 0.95 s instead of 0.09 s, and the 200 OK at 3.6 s instead of 2.7 s. After
// the 180 the firmware mostly waits for the camera and the first audio and video
// are seconds away, so nothing is lost by then.
//
// The start waits for the first of the 180 and the AV request. A ringing preview
// sees no 180 of ours, as nothing is dialled, so its pipelines start at its AV
// request, where they always did.
type streamPipelines struct {
	mu        sync.Mutex
	run       func(context.Context) error
	lifecycle context.Context
	started   bool
	closed    bool
	err       error
}

func newStreamPipelines(run func(context.Context) error) *streamPipelines {
	return &streamPipelines{run: run}
}

// arm gives the pipelines the context of the stream, which ends them.
func (p *streamPipelines) arm(lifecycle context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.lifecycle = lifecycle
}

// start runs the pipelines the first time and reports how that went every time.
// It does nothing before arm, as a response that comes earlier is not this
// stream's.
func (p *streamPipelines) start() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch {
	case p.closed:
		return errStreamPipelinesClosed
	case p.lifecycle == nil:
		return nil
	case !p.started:
		p.started = true
		p.err = p.run(p.lifecycle)
	}

	return p.err
}

// close stops a later start from running anything, and waits for one in progress:
// what stops the pipelines next then finds them all.
func (p *streamPipelines) close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true
}

// pipelineAV starts the pipelines, if the 180 has not, right before the intercom is
// asked for media. StartAudio needs nothing: it only completes a ringing preview,
// whose pipelines started with its video.
type pipelineAV struct {
	media.SourceAV

	pipelines *streamPipelines
}

func (a pipelineAV) Start(ctx context.Context, highRes bool, ports media.AVPorts, video, audio media.FlowProbe) error {
	if err := a.pipelines.start(); err != nil {
		return err
	}

	return a.SourceAV.Start(ctx, highRes, ports, video, audio)
}

func (a pipelineAV) StartVideo(ctx context.Context, highRes bool, port int, probe media.FlowProbe) error {
	if err := a.pipelines.start(); err != nil {
		return err
	}

	return a.SourceAV.StartVideo(ctx, highRes, port, probe)
}

package app

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bticino-go-companion/internal/media"
	"bticino-go-companion/internal/openwebnet"

	"github.com/pion/rtp"
)

const (
	// ringVideoFallback is how long the ring's video request waits for the
	// intercom's own streams before asking anyway.
	ringVideoFallback = 3 * time.Second
	// ringTimelineWindow bounds a ring's timeline: whatever has not happened by
	// then is reported as missing.
	ringTimelineWindow = 10 * time.Second
	// ringFrameRepeat folds the 2–4 copies the intercom sends of each AV frame.
	ringFrameRepeat = 100 * time.Millisecond
)

// ringVideoWatch follows the AV frames of each ring. It tells the ringing
// preview when the intercom has started its own streams, so the video request
// goes out only then, and logs one timeline line per ring to measure it. See
// docs/analysis/2026-10-29-ring-snapshot-latency.md, P4.
type ringVideoWatch struct {
	logger   *slog.Logger
	now      func() time.Time
	fallback time.Duration
	window   time.Duration

	mu sync.Mutex
	// generation changes on every ring, so a late report or request knows its
	// ring is gone.
	generation uint64
	ringAt     time.Time
	// streamUp is closed when the intercom starts its own stream for this ring.
	streamUp chan struct{}
	timeline ringTimeline
}

type ringTimeline struct {
	activations    []time.Duration
	parameters     time.Duration
	intercomStream time.Duration
	request        time.Duration
	requestReason  string
	videoStarted   time.Duration
	videoErr       error
	firstRTP       time.Duration
	reported       bool
}

func newRingVideoWatch(logger *slog.Logger) *ringVideoWatch {
	return &ringVideoWatch{
		logger:   logger.With("component", "ring_video"),
		now:      time.Now,
		fallback: ringVideoFallback,
		window:   ringTimelineWindow,
		streamUp: make(chan struct{}),
	}
}

// observeFrame takes every OpenWebNet frame, before it is mapped to events.
func (w *ringVideoWatch) observeFrame(frame string) {
	now := w.now()

	w.mu.Lock()
	defer w.mu.Unlock()

	if openwebnet.IsRingStart(frame) {
		w.startRing(now)
		return
	}

	if w.ringAt.IsZero() {
		return
	}

	offset := now.Sub(w.ringAt)

	switch {
	case openwebnet.IsAVActivation(frame):
		if n := len(w.timeline.activations); n == 0 || offset-w.timeline.activations[n-1] > ringFrameRepeat {
			w.timeline.activations = append(w.timeline.activations, offset)
		}
	case openwebnet.IsAVParameters(frame):
		if w.timeline.parameters == 0 {
			w.timeline.parameters = offset
		}
	case openwebnet.IsIntercomStreamStart(frame):
		if w.timeline.intercomStream == 0 {
			w.timeline.intercomStream = offset
			close(w.streamUp)
		}
	}
}

func (w *ringVideoWatch) startRing(now time.Time) {
	w.reportLocked("next ring")

	w.generation++
	w.ringAt = now
	w.streamUp = make(chan struct{})
	w.timeline = ringTimeline{}

	generation := w.generation
	time.AfterFunc(w.window, func() {
		w.mu.Lock()
		defer w.mu.Unlock()

		if w.generation == generation {
			w.reportLocked("window elapsed")
		}
	})
}

// waitIntercomStream blocks until the intercom has started its own stream for
// the current ring, or the fallback elapses. It reports why it returned.
func (w *ringVideoWatch) waitIntercomStream(ctx context.Context) string {
	w.mu.Lock()
	streamUp := w.streamUp
	w.mu.Unlock()

	select {
	case <-streamUp:
		return "intercom_stream"
	default:
	}

	timer := time.NewTimer(w.fallback)
	defer timer.Stop()

	select {
	case <-streamUp:
		return "intercom_stream"
	case <-timer.C:
		return "fallback"
	case <-ctx.Done():
		return "cancelled"
	}
}

func (w *ringVideoWatch) record(update func(*ringTimeline, time.Duration)) {
	now := w.now()

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.ringAt.IsZero() || w.timeline.reported {
		return
	}

	update(&w.timeline, now.Sub(w.ringAt))

	if w.timeline.firstRTP != 0 && (w.timeline.videoStarted != 0 || w.timeline.videoErr != nil) {
		w.reportLocked("complete")
	}
}

func (w *ringVideoWatch) reportLocked(reason string) {
	if w.ringAt.IsZero() || w.timeline.reported {
		return
	}

	w.timeline.reported = true
	t := w.timeline

	activations := make([]string, len(t.activations))
	for i, offset := range t.activations {
		activations[i] = strconv.FormatInt(offset.Milliseconds(), 10)
	}

	w.logger.Info("ring video timeline",
		"reason", reason,
		"activation_ms", strings.Join(activations, ","),
		"parameters_ms", offsetMS(t.parameters),
		"intercom_stream_ms", offsetMS(t.intercomStream),
		"request_ms", offsetMS(t.request),
		"request_reason", t.requestReason,
		"video_started_ms", offsetMS(t.videoStarted),
		"video_error", t.videoErr,
		"first_rtp_ms", offsetMS(t.firstRTP),
	)
}

// offsetMS renders an offset from the ring, or -1 for what never happened.
func offsetMS(offset time.Duration) int64 {
	if offset == 0 {
		return -1
	}

	return offset.Milliseconds()
}

// sourceAV wraps the AV client of a media source. Only a ringing preview asks
// for video alone, so StartVideo is the ring's request: it waits for the
// intercom's own streams first.
func (w *ringVideoWatch) sourceAV(av media.SourceAV) media.SourceAV {
	return ringSourceAV{SourceAV: av, watch: w}
}

// clip wraps the ring holder's clip to stamp the ring's first video packet.
func (w *ringVideoWatch) clip(clip ringClip) ringClip {
	return &ringTimelineClip{ringClip: clip, watch: w}
}

type ringSourceAV struct {
	media.SourceAV
	watch *ringVideoWatch
}

func (a ringSourceAV) StartVideo(ctx context.Context, highRes bool, port int, probe media.FlowProbe) error {
	reason := a.watch.waitIntercomStream(ctx)
	a.watch.logger.DebugContext(ctx, "ring video request released", "reason", reason)

	if err := ctx.Err(); err != nil {
		return err
	}

	// The ring holder asks first; a card that opens its own preview later in
	// the same ring does not overwrite it.
	first := false

	a.watch.record(func(t *ringTimeline, offset time.Duration) {
		if t.request == 0 {
			first = true
			t.request, t.requestReason = offset, reason
		}
	})

	err := a.SourceAV.StartVideo(ctx, highRes, port, probe)

	if first {
		a.watch.record(func(t *ringTimeline, offset time.Duration) {
			if err != nil {
				t.videoErr = err
			} else {
				t.videoStarted = offset
			}
		})
	}

	return err
}

type ringTimelineClip struct {
	ringClip
	watch *ringVideoWatch
	seen  atomic.Bool
}

func (c *ringTimelineClip) VideoRTP(packet *rtp.Packet) {
	if c.seen.CompareAndSwap(false, true) {
		c.watch.record(func(t *ringTimeline, offset time.Duration) {
			if t.firstRTP == 0 {
				t.firstRTP = offset
			}
		})
	}

	c.ringClip.VideoRTP(packet)
}

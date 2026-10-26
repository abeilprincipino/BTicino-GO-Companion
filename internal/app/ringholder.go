package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"

	"github.com/pion/rtp"
)

// ringHolderStartupTimeout bounds the stream startup the holder waits for: the
// same budget a card's offer gets.
const ringHolderStartupTimeout = 15 * time.Second

// ringHolderCoordinator is the part of the stream coordinator the ring holder
// uses.
type ringHolderCoordinator interface {
	AcquireRingingPreview(lifecycleCtx, startupCtx context.Context, entrypoint config.Entrypoint, events media.SourceEvents) (*media.StreamLease, error)
	Leave(*media.StreamLease) bool
}

// ringClip is the recording the holder feeds while it holds the stream:
// *media.RingClip. See docs/superpowers/specs/2026-10-26-ring-clip-design.md.
type ringClip interface {
	VideoRTP(*rtp.Packet)
	Close()
}

// ringHolder opens the intercom stream on every SIP ring. The companion
// receives video only when it asks for it, so without this no doorbell picture
// is taken while no card is open. It keeps the stream until the answer or the
// end of the ring, so a card opening later joins it rather than asking the
// intercom for video on a new port mid-ring, and it records the first seconds
// of the ring as a clip. See
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
type ringHolder struct {
	coordinator ringHolderCoordinator
	startClip   func(entrypointID string) ringClip
	entrypoints func() []config.Entrypoint
	logger      *slog.Logger

	mu sync.Mutex
	// generation changes on every ring and every end of one, so an acquire
	// that completes late knows its ring is gone.
	generation uint64
	lease      *media.StreamLease
	// clip records the ring while the holder has the stream. It is closed with
	// the lease, so the answer ends the recording.
	clip ringClip
}

func newRingHolder(coordinator ringHolderCoordinator, startClip func(entrypointID string) ringClip, entrypoints func() []config.Entrypoint, logger *slog.Logger) *ringHolder {
	return &ringHolder{coordinator: coordinator, startClip: startClip, entrypoints: entrypoints, logger: logger.With("component", "ring_holder")}
}

// route follows the call events and passes every event on to next. The answer
// is seen after PromoteRinging when routeRingingPreview wraps this, so a card
// on the stream is promoted before the holder steps away.
func (h *ringHolder) route(next func(core.Event)) func(core.Event) {
	return func(event core.Event) {
		switch event := event.(type) {
		case core.IncomingCallStarted:
			h.hold(event.EntrypointID)
		case core.CallAnswered, core.IncomingCallEnded, core.CallDeclined, core.CallHungUp:
			h.release()
		}

		next(event)
	}
}

func (h *ringHolder) hold(entrypointID core.EntrypointID) {
	h.mu.Lock()
	h.generation++
	generation := h.generation
	previous, previousClip := h.lease, h.clip
	h.lease, h.clip = nil, nil
	h.mu.Unlock()

	if previousClip != nil {
		previousClip.Close()
	}

	if previous != nil {
		h.coordinator.Leave(previous)
	}

	entrypoint, ok := h.entrypoint(entrypointID)
	if !ok {
		h.logger.Debug("ring holder skipped; the entrypoint cannot stream", "entrypoint_id", entrypointID)
		return
	}

	go h.acquire(generation, entrypoint)
}

func (h *ringHolder) acquire(generation uint64, entrypoint config.Entrypoint) {
	startupCtx, cancel := context.WithTimeout(context.Background(), ringHolderStartupTimeout)
	defer cancel()

	clip := h.startClip(entrypoint.ID)
	events := media.SourceEvents{
		VideoRTP:  clip.VideoRTP,
		RemoteBYE: clip.Close,
		Failed:    func(error) { clip.Close() },
	}

	lease, err := h.coordinator.AcquireRingingPreview(context.Background(), startupCtx, entrypoint, events)
	if err != nil {
		clip.Close()
		h.logger.Warn("ring holder could not open the stream; no doorbell picture or clip for this ring", "entrypoint_id", entrypoint.ID, "error", err)

		return
	}

	h.mu.Lock()
	current := h.generation == generation
	if current {
		h.lease, h.clip = lease, clip
	}
	h.mu.Unlock()

	// The ring was answered or ended while the stream started.
	if !current {
		clip.Close()
		h.coordinator.Leave(lease)
	}
}

func (h *ringHolder) release() {
	h.mu.Lock()
	h.generation++
	lease, clip := h.lease, h.clip
	h.lease, h.clip = nil, nil
	h.mu.Unlock()

	if clip != nil {
		clip.Close()
	}

	if lease != nil {
		h.coordinator.Leave(lease)
	}
}

func (h *ringHolder) entrypoint(id core.EntrypointID) (config.Entrypoint, bool) {
	for _, entrypoint := range h.entrypoints() {
		if core.EntrypointID(entrypoint.ID) == id && entrypoint.Capabilities.Stream {
			return entrypoint, true
		}
	}

	return config.Entrypoint{}, false
}

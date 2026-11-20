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
	RetargetStream(from string, to config.Entrypoint) bool
}

// ringClip is the recording the holder feeds while it holds the stream:
// *media.RingClip. See docs/superpowers/specs/2026-10-26-ring-clip-design.md.
type ringClip interface {
	VideoRTP(*rtp.Packet)
	// Retarget makes the clip another entrypoint's.
	Retarget(entrypointID string)
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
	// opened is the entrypoint the ring's stream was opened for, ring the one
	// the ring belongs to, and streamAt the one the stream and the clip are
	// labelled with. ring moves when a C100X names the external unit that
	// rang after the ring frame; see retarget. They keep their values after
	// the ring, since a picture may still be on its way, until the next ring.
	opened, ring, streamAt core.EntrypointID
	// moveMu runs the relabelling of the stream one move at a time.
	moveMu sync.Mutex
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
		case core.RingRetargeted:
			h.retarget(event.From, event.To)
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
	h.opened, h.ring, h.streamAt = entrypointID, entrypointID, entrypointID
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

		return
	}

	// The ring may have moved while the stream started.
	h.move()
}

// retarget follows the ring from entrypoint from to entrypoint to: a C100X
// sends the ring frame with one external unit's address and names the unit
// that rang in a later frame. The stream stays as it is, since the intercom
// sends the camera that rang, but the clip, the doorbell picture and the
// stream are labelled with to from now on.
func (h *ringHolder) retarget(from, to core.EntrypointID) {
	h.mu.Lock()
	if h.ring == "" || h.ring != from || to == "" {
		h.mu.Unlock()

		return
	}

	h.ring = to
	streaming := h.lease != nil
	h.mu.Unlock()

	h.logger.Info("ring moved to the entrypoint that rang", "from_entrypoint_id", from, "entrypoint_id", to, "stream_open", streaming)

	h.move()
}

// move labels the held stream and its clip with the entrypoint the ring
// belongs to now. Without a held stream it waits for acquire to call it.
func (h *ringHolder) move() {
	h.moveMu.Lock()
	defer h.moveMu.Unlock()

	h.mu.Lock()
	lease, clip, target, at := h.lease, h.clip, h.ring, h.streamAt
	h.mu.Unlock()

	if lease == nil || target == at {
		return
	}

	if clip != nil {
		clip.Retarget(string(target))
	}

	if entrypoint, ok := h.entrypoint(target); ok && !h.coordinator.RetargetStream(string(at), entrypoint) {
		h.logger.Debug("ring stream not moved; it is no longer the ring's", "from_entrypoint_id", at, "entrypoint_id", target)
	}

	h.mu.Lock()
	if h.lease == lease {
		h.streamAt = target
	}
	h.mu.Unlock()
}

// ringLabel names the entrypoint a doorbell picture of a stream opened for
// entrypointID belongs to: the one the ring moved to, if it moved.
func (h *ringHolder) ringLabel(entrypointID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.ring != "" && string(h.opened) == entrypointID {
		return string(h.ring)
	}

	return entrypointID
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

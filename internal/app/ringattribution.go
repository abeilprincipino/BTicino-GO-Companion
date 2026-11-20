package app

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
)

// heldPublisher applies every event to the projector at once and publishes the
// result: HomeKit and the API's state and events. It can hold the publishing
// back for a while, so a ring whose doorbell is still being settled is
// announced once, settled. Everything inside the companion keeps working on
// the projector meanwhile: the inbound call is attributed, and the ring's
// stream, picture and clip start, without waiting.
type heldPublisher struct {
	projector *core.Projector
	publish   func(state core.State, events []core.EventType)
	logger    *slog.Logger

	mu sync.Mutex
	// held is set while publishing is held back, pending the events applied
	// meanwhile, and generation names the hold, so a late timer of an old one
	// cannot end a newer one.
	held       bool
	pending    []core.EventType
	generation uint64
	heldAt     time.Time
	timer      *time.Timer
}

func newHeldPublisher(projector *core.Projector, publish func(core.State, []core.EventType), logger *slog.Logger) *heldPublisher {
	return &heldPublisher{projector: projector, publish: publish, logger: logger.With("component", "ring_attribution")}
}

func (p *heldPublisher) apply(event core.Event) {
	if _, err := p.projector.Apply(event); err != nil && !errors.Is(err, core.ErrInvalidTransition) {
		p.logger.Warn("openwebnet event apply failed", "event_type", event.Type(), "error", err)
		return
	}

	p.mu.Lock()
	if p.held {
		p.pending = append(p.pending, event.Type())
		p.mu.Unlock()

		return
	}
	p.mu.Unlock()

	p.publish(p.projector.Snapshot(), []core.EventType{event.Type()})
}

// hold holds publishing back for up to wait. A hold already running keeps its
// own end.
func (p *heldPublisher) hold(wait time.Duration) {
	if wait <= 0 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.held {
		return
	}

	p.held = true
	p.generation++
	p.heldAt = time.Now()
	generation := p.generation
	p.timer = time.AfterFunc(wait, func() { p.end(generation, "timeout") })
}

// release publishes what was held back, if anything is.
func (p *heldPublisher) release(reason string) {
	p.mu.Lock()
	generation := p.generation
	p.mu.Unlock()

	p.end(generation, reason)
}

func (p *heldPublisher) end(generation uint64, reason string) {
	p.mu.Lock()
	if !p.held || p.generation != generation {
		p.mu.Unlock()
		return
	}

	p.held = false
	pending := p.pending
	p.pending = nil
	heldFor := time.Since(p.heldAt)

	p.timer.Stop()
	p.mu.Unlock()

	p.logger.Debug("ring report released", "reason", reason, "held_ms", heldFor.Milliseconds(), "events", len(pending))
	p.publish(p.projector.Snapshot(), pending)
}

// ringAttributionLearner is the part of ringattribution.Learner the bus uses.
type ringAttributionLearner interface {
	Hold(address string) time.Duration
	Observe(address, entrypointID string, delay time.Duration)
}

// ringPublicationHold is the part of heldPublisher the bus uses.
type ringPublicationHold interface {
	hold(wait time.Duration)
	release(reason string)
}

// routeRingAttribution holds the report of a ring whose frame carries an
// address that misled before, for as long as learned, and ends the hold when
// the intercom names the unit that rang: the ring, moved by RingRetargeted
// meanwhile if it has to, is then reported once, already right. Every
// correction is learned. RingIdentified changes no state and goes no further.
func routeRingAttribution(learner ringAttributionLearner, publication ringPublicationHold, logger *slog.Logger, next func(core.Event)) func(core.Event) {
	return func(event core.Event) {
		switch event := event.(type) {
		case core.RingStarted:
			if wait := learner.Hold(event.FrameAddress); wait > 0 {
				logger.Debug("ring report held until the intercom names the unit", "component", "ring_attribution",
					"address", event.FrameAddress, "entrypoint_id", event.EntrypointID, "hold_ms", wait.Milliseconds())
				publication.hold(wait)
			}

			next(event)
		case core.RingIdentified:
			if event.Corrected() {
				learner.Observe(event.FrameAddress, string(event.EntrypointID), event.Delay)
			}

			publication.release("identified")
		default:
			next(event)
		}
	}
}

// ringAttributionSettings reads the learning settings from the configuration
// as it is now, so a change in the Web UI applies to the next ring.
func ringAttributionSettings(configStore *config.Store) func() (bool, time.Duration) {
	return func() (bool, time.Duration) {
		settings := configStore.Snapshot().System.RingAttribution
		return settings.Enabled, settings.MaxWait()
	}
}

// configuredDoorbells lists the entrypoints that can ring now.
func configuredDoorbells(configStore *config.Store) func() []string {
	return func() []string {
		var doorbells []string

		for _, entrypoint := range configStore.Snapshot().Companion.Entrypoints {
			if entrypoint.Capabilities.Ring {
				doorbells = append(doorbells, entrypoint.ID)
			}
		}

		return doorbells
	}
}

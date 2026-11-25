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
//
// More than one hold can run at once, each with its owner: the ring
// attribution's (hold, release) and the repeat ring guard's. Publishing
// resumes when the last one ends.
type heldPublisher struct {
	projector *core.Projector
	publish   func(state core.State, events []core.EventType)
	logger    *slog.Logger

	mu sync.Mutex
	// holds are the running holds by owner, and pending the events applied
	// while any runs. generation names each hold, so a late timer of an old
	// one cannot end a newer one.
	holds      map[string]*publicationHold
	pending    []core.EventType
	generation uint64
}

type publicationHold struct {
	generation uint64
	heldAt     time.Time
	timer      *time.Timer
}

// The owners of a hold.
const (
	holdRingAttribution = "ring_attribution"
	holdRepeatRing      = "repeat_ring"
)

func newHeldPublisher(projector *core.Projector, publish func(core.State, []core.EventType), logger *slog.Logger) *heldPublisher {
	return &heldPublisher{projector: projector, publish: publish, logger: logger.With("component", "ring_attribution"), holds: map[string]*publicationHold{}}
}

func (p *heldPublisher) apply(event core.Event) {
	if _, err := p.projector.Apply(event); err != nil && !errors.Is(err, core.ErrInvalidTransition) {
		p.logger.Warn("openwebnet event apply failed", "event_type", event.Type(), "error", err)
		return
	}

	p.mu.Lock()
	if len(p.holds) > 0 {
		p.pending = append(p.pending, event.Type())
		p.mu.Unlock()

		return
	}
	p.mu.Unlock()

	p.publish(p.projector.Snapshot(), []core.EventType{event.Type()})
}

// hold holds publishing back for up to wait, for the ring attribution. A hold
// already running keeps its own end.
func (p *heldPublisher) hold(wait time.Duration) { p.holdAs(holdRingAttribution, wait) }

// release ends the ring attribution's hold.
func (p *heldPublisher) release(reason string) { p.releaseAs(holdRingAttribution, reason, nil) }

// holdAs holds publishing back for up to wait on behalf of owner. A hold of
// the same owner already running keeps its own end.
func (p *heldPublisher) holdAs(owner string, wait time.Duration) {
	if wait <= 0 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, held := p.holds[owner]; held {
		return
	}

	p.generation++
	generation := p.generation
	p.holds[owner] = &publicationHold{
		generation: generation,
		heldAt:     time.Now(),
		timer:      time.AfterFunc(wait, func() { p.end(owner, generation, "timeout", nil) }),
	}
}

// releaseAs ends owner's hold. drop, if set, takes out the events held back
// so far that are not to be told; the state is published as it is.
func (p *heldPublisher) releaseAs(owner, reason string, drop func(core.EventType) bool) {
	p.mu.Lock()
	hold, held := p.holds[owner]
	p.mu.Unlock()

	if held {
		p.end(owner, hold.generation, reason, drop)
	}
}

func (p *heldPublisher) isHeld() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.holds) > 0
}

func (p *heldPublisher) end(owner string, generation uint64, reason string, drop func(core.EventType) bool) {
	p.mu.Lock()

	hold, held := p.holds[owner]
	if !held || hold.generation != generation {
		p.mu.Unlock()
		return
	}

	delete(p.holds, owner)
	hold.timer.Stop()

	if drop != nil {
		kept := p.pending[:0]

		for _, event := range p.pending {
			if !drop(event) {
				kept = append(kept, event)
			}
		}

		p.pending = kept
	}

	heldFor := time.Since(hold.heldAt)

	if len(p.holds) > 0 {
		p.mu.Unlock()
		p.logger.Debug("ring report hold ended; another still holds it", "owner", owner, "reason", reason, "held_ms", heldFor.Milliseconds())

		return
	}

	pending := p.pending
	p.pending = nil
	p.mu.Unlock()

	p.logger.Debug("ring report released", "owner", owner, "reason", reason, "held_ms", heldFor.Milliseconds(), "events", len(pending))
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

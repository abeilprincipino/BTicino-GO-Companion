package app

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/openwebnet"
)

// repeatRingWindow is how long after a street panel ended its own call the
// guard waits for that panel to call again. On a C100X on 2026-11-21/22 the new
// ring frame came 0.3 s after the end and its INVITE 1 s after; the decline it
// may need is over within another half second.
const repeatRingWindow = 4 * time.Second

// repeatRingTimeout bounds the decline of a repeated call.
const repeatRingTimeout = 5 * time.Second

// repeatRingPublication is the part of heldPublisher the guard uses.
type repeatRingPublication interface {
	holdAs(owner string, wait time.Duration)
	releaseAs(owner, reason string, drop func(core.EventType) bool)
}

// repeatRingGuard handles a street panel that, 10 s or so into an unanswered
// ring, ends the call itself (*8*3#1#3*4<panel>##) and calls the same indoor
// unit again at once, with a new ring frame and a new INVITE: the house rings
// twice for one visitor. For an entrypoint with ignore_repeat_ring, the guard
// holds back the report of the end, and when the panel calls again:
//
//   - if the door of that entrypoint was opened during the first call, it
//     declines the new call, so the house stops ringing, and the clients hear
//     only that the call ended;
//   - otherwise the visitor may still be waiting, so the call goes on, and the
//     clients hear nothing: to them the first call has not ended.
//
// If the panel does not call again within repeatRingWindow, the end is
// reported then. An answered call, or one the indoor unit ended, is not
// touched. See docs/superpowers/specs/2026-11-22-repeat-ring-design.md.
type repeatRingGuard struct {
	entrypoints func() []config.Entrypoint
	publication repeatRingPublication
	hangup      func(context.Context) error
	now         func() time.Time
	logger      *slog.Logger

	mu sync.Mutex
	// ringing, answered and unlocked follow the ring call on the bus:
	// unlocked holds the addresses of the doors opened during it. A repeated
	// call keeps them, since it is the same visitor.
	ringing, answered bool
	unlocked          map[string]bool
	// entrypoint is set while a repeated call of it is expected, until
	// until; opened says the door was opened during the first call.
	entrypoint string
	until      time.Time
	opened     bool
	// repeat is the dialog of the repeated call, declining set while the
	// guard declines it.
	repeat    core.DialogID
	declining bool
}

func newRepeatRingGuard(entrypoints func() []config.Entrypoint, publication repeatRingPublication, hangup func(context.Context) error, logger *slog.Logger) *repeatRingGuard {
	return &repeatRingGuard{
		entrypoints: entrypoints,
		publication: publication,
		hangup:      hangup,
		now:         time.Now,
		logger:      logger.With("component", "repeat_ring"),
		unlocked:    map[string]bool{},
	}
}

// observeFrame takes every OpenWebNet frame, before the events it maps to.
func (g *repeatRingGuard) observeFrame(frame string) {
	if callType, _, ok := openwebnet.ParseCallStart(frame); ok {
		if callType == "1" {
			g.callStarted()
		}

		return
	}

	if callType, mode, address, ok := openwebnet.ParseCallEnd(frame); ok {
		if callType == "1" {
			g.callEnded(mode, address)
		}

		return
	}

	trimmed := strings.TrimSpace(frame)

	switch {
	case strings.HasPrefix(trimmed, "*8*2#1#"):
		g.mu.Lock()
		g.answered = g.ringing
		g.mu.Unlock()
	case openwebnet.IsUnlockOpen(trimmed):
		address := strings.TrimSuffix(strings.TrimPrefix(trimmed, "*8*19*"), "##")

		g.mu.Lock()
		if g.ringing {
			g.unlocked[address] = true
		}
		g.mu.Unlock()
	}
}

func (g *repeatRingGuard) callStarted() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.ringing {
		return
	}

	// A repeated call is the same visitor: what was done during the first
	// call still holds.
	if !g.expecting() {
		g.answered = false
		g.unlocked = map[string]bool{}
	}

	g.ringing = true
}

func (g *repeatRingGuard) callEnded(mode, address string) {
	g.mu.Lock()

	if !g.ringing {
		g.mu.Unlock()
		return
	}

	g.ringing = false

	// Only the panel's own end of an unanswered call is followed by another.
	if g.answered || mode != "3" {
		g.mu.Unlock()
		return
	}

	entrypoint, ok := g.guarded(address)
	if !ok {
		g.mu.Unlock()
		return
	}

	g.entrypoint, g.until, g.opened = entrypoint.ID, g.now().Add(repeatRingWindow), g.unlocked[address]
	g.repeat, g.declining = "", false
	opened := g.opened
	g.mu.Unlock()

	g.logger.Info("street panel ended its call; holding the report for a repeated call", "entrypoint_id", entrypoint.ID, "door_opened", opened)
	g.publication.holdAs(holdRepeatRing, repeatRingWindow)
}

// guarded finds the entrypoint with ignore_repeat_ring whose panel has address.
func (g *repeatRingGuard) guarded(address string) (config.Entrypoint, bool) {
	for _, entrypoint := range g.entrypoints() {
		if entrypoint.IgnoreRepeatRing && entrypoint.DevAddr != "" && entrypoint.DevAddr == address {
			return entrypoint, true
		}
	}

	return config.Entrypoint{}, false
}

// expecting reports whether a repeated call is due now. g.mu must be held.
func (g *repeatRingGuard) expecting() bool {
	return g.entrypoint != "" && g.repeat == "" && g.now().Before(g.until)
}

// routeCalls follows the SIP call events, before they reach next.
func (g *repeatRingGuard) routeCalls(next func(core.Event)) func(core.Event) {
	return func(event core.Event) {
		started, ok := event.(core.IncomingCallStarted)
		if !ok {
			next(event)
			return
		}

		g.mu.Lock()
		if !g.expecting() {
			g.mu.Unlock()
			next(event)

			return
		}

		g.repeat = started.DialogID
		g.declining = g.opened
		decline, entrypoint := g.declining, g.entrypoint
		g.mu.Unlock()

		next(event)

		if !decline {
			g.logger.Info("street panel called again; the call goes on as the first one", "entrypoint_id", entrypoint, "dialog_id", started.DialogID)
			g.publication.releaseAs(holdRepeatRing, "repeat merged", isRingRestart)

			return
		}

		g.logger.Info("street panel called again after the door was opened; declining the call", "entrypoint_id", entrypoint, "dialog_id", started.DialogID)

		go g.decline(entrypoint)
	}
}

func (g *repeatRingGuard) decline(entrypoint string) {
	ctx, cancel := context.WithTimeout(context.Background(), repeatRingTimeout)
	defer cancel()

	if err := g.hangup(ctx); err != nil {
		g.logger.Warn("repeated call decline failed; reporting it", "entrypoint_id", entrypoint, "error", err)

		g.mu.Lock()
		g.declining = false
		g.mu.Unlock()

		g.publication.releaseAs(holdRepeatRing, "decline failed", nil)
	}
}

// routeBus follows the bus events, before they reach next: the ring cleared
// after the decline is the end of the repeated call, and the report goes out
// as the end of the first one.
func (g *repeatRingGuard) routeBus(next func(core.Event)) func(core.Event) {
	return func(event core.Event) {
		next(event)

		if _, cleared := event.(core.RingCleared); !cleared {
			return
		}

		g.mu.Lock()
		declined := g.declining
		g.declining = false
		g.mu.Unlock()

		if declined {
			g.publication.releaseAs(holdRepeatRing, "repeat declined", newRepeatDeclineDrop())
		}
	}
}

// isRingRestart picks the events of the first call's end and of the repeated
// call's start: for a call that goes on, the clients hear neither.
func isRingRestart(event core.EventType) bool {
	switch event {
	case core.EventRingCleared, core.EventIncomingCallEnded, core.EventRingStarted, core.EventIncomingCallStarted,
		core.EventRingRetargeted, core.EventRingIdentified:
		return true
	default:
		return false
	}
}

// newRepeatDeclineDrop picks the events of a declined repeated call: its start
// and decline, and the second report of an end, so the clients hear the first
// call end once.
func newRepeatDeclineDrop() func(core.EventType) bool {
	seen := map[core.EventType]bool{}

	return func(event core.EventType) bool {
		switch event {
		case core.EventRingStarted, core.EventIncomingCallStarted, core.EventCallDeclined,
			core.EventRingRetargeted, core.EventRingIdentified:
			return true
		case core.EventRingCleared, core.EventIncomingCallEnded:
			if seen[event] {
				return true
			}

			seen[event] = true
		}

		return false
	}
}

// Package ringersync keeps the intercom's display in step with a ringer
// unmuted by the companion.
//
// The display's GUI follows a mute it sees on the bus but ignores an unmute
// (seen on a C100X on 2026-11-05; c300x-controller works around the same on
// the C300X): after an unmute from Home Assistant or HomeKit the display goes
// on showing the ringer muted. Only a firmware reload makes the GUI read the
// state again, and that reload stops the whole firmware for about 25 s, so it
// runs once the commands have settled and the intercom is idle.
package ringersync

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// AudioControl is the intercom's ringer mute.
type AudioControl interface {
	Mute(ctx context.Context) error
	Unmute(ctx context.Context) error
}

type Config struct {
	Audio AudioControl
	// Muted reports the ringer's current state, read before an unmute: a
	// ringer that was already on needs no reload.
	Muted func() bool
	// Busy reports a ring, a call or a stream, which a reload would cut.
	Busy func() bool
	// Reload restarts the firmware so the GUI reads the ringer state again.
	Reload func(context.Context) error
	// Delay is how long the commands must settle before a reload.
	Delay time.Duration
	// Poll is how often Busy is asked while the reload waits.
	Poll   time.Duration
	Logger *slog.Logger
}

// Sync wraps the ringer control: an unmute of a muted ringer schedules a
// firmware reload, which a later mute cancels.
type Sync struct {
	cfg  Config
	wake chan struct{}

	mu      sync.Mutex
	pending bool
	due     time.Time
}

func New(cfg Config) *Sync {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return &Sync{cfg: cfg, wake: make(chan struct{}, 1)}
}

func (s *Sync) Mute(ctx context.Context) error {
	if err := s.cfg.Audio.Mute(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	s.pending = false
	s.mu.Unlock()

	return nil
}

func (s *Sync) Unmute(ctx context.Context) error {
	wasMuted := s.cfg.Muted == nil || s.cfg.Muted()

	if err := s.cfg.Audio.Unmute(ctx); err != nil {
		return err
	}

	if !wasMuted {
		return nil
	}

	s.mu.Lock()
	s.pending = true
	s.due = time.Now().Add(s.cfg.Delay)
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}

	return nil
}

// Run performs the scheduled reloads until ctx ends.
func (s *Sync) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}

		if !s.waitUntilDue(ctx) {
			return
		}
	}
}

// waitUntilDue waits for the last unmute to settle and the intercom to be
// idle, then reloads. It returns false when ctx ends.
func (s *Sync) waitUntilDue(ctx context.Context) bool {
	ticker := time.NewTicker(s.cfg.Poll)
	defer ticker.Stop()

	logged := false

	for {
		select {
		case <-ctx.Done():
			return false
		case <-s.wake:
		case <-ticker.C:
		}

		s.mu.Lock()
		pending, due := s.pending, s.due
		s.mu.Unlock()

		if !pending {
			return true
		}

		if time.Now().Before(due) {
			continue
		}

		if s.cfg.Busy != nil && s.cfg.Busy() {
			if !logged {
				s.cfg.Logger.Info("display reload after unmute waits for the intercom to be idle")
				logged = true
			}

			continue
		}

		s.mu.Lock()
		s.pending = false
		s.mu.Unlock()

		s.cfg.Logger.Info("reloading the intercom firmware so its display shows the ringer unmuted")

		if err := s.cfg.Reload(ctx); err != nil {
			s.cfg.Logger.Warn("display reload after unmute failed", "error", err)
		}

		return true
	}
}

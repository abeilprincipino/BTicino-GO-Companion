package openwebnet

import (
	"context"
	"strings"
	"sync"
	"time"
)

// AVReadiness tracks whether the intercom has learned the outdoor unit's video
// parameters for the current ring. At a ring the intercom announces them in a
// *7*77 frame about 0.15 s after the companion would first ask for video; a
// request sent before that frame is held for ~1.4 s and refused, and the
// accepted retry lands ~2.4 s after the ring. Waiting for the frame first is
// how the request is meant to be accepted on its first attempt.
type AVReadiness struct {
	mu     sync.Mutex
	ready  bool
	notify chan struct{}
}

func NewAVReadiness() *AVReadiness {
	return &AVReadiness{notify: make(chan struct{})}
}

// Observe takes every frame the bus listener reads.
func (r *AVReadiness) Observe(frame string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case IsRingStart(frame):
		// A new ring: the parameters of the previous one say nothing.
		if r.ready {
			r.ready = false
			r.notify = make(chan struct{})
		}
	case IsVideoParameters(frame):
		if !r.ready {
			r.ready = true
			close(r.notify)
		}
	}
}

// WaitVideoParameters returns once the current ring's video parameters are
// known, or after max, whichever comes first. It reports whether they are.
func (r *AVReadiness) WaitVideoParameters(ctx context.Context, max time.Duration) bool {
	r.mu.Lock()
	ready, notify := r.ready, r.notify
	r.mu.Unlock()

	if ready {
		return true
	}

	timer := time.NewTimer(max)
	defer timer.Stop()

	select {
	case <-notify:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// IsVideoParameters reports a *7*77 frame: the outdoor unit's video format,
// which the intercom announces at a ring before it accepts a video request.
func IsVideoParameters(frame string) bool {
	return strings.HasPrefix(strings.TrimSpace(frame), "*7*77#")
}

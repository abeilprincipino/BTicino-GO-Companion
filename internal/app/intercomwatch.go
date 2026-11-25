package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"syscall"
	"time"

	"bticino-go-companion/internal/core"
)

const (
	// intercomProbeInterval is how often the watch asks the bus while the
	// intercom is unavailable.
	intercomProbeInterval = 2 * time.Second
	// intercomProbeTimeout bounds one question to the bus.
	intercomProbeTimeout = 3 * time.Second
	// intercomStopWait bounds how long after a firmware reload starts the
	// watch waits for the bus to stop answering. On a C100X on 2026-11-22 the
	// SIP registrar closed 4.7 s after the reload and the bus refused
	// connections from 11 s on.
	intercomStopWait = 20 * time.Second
	// intercomProbesToRecover is how many answers in a row make the intercom
	// available again: while it starts, the bus answers some commands and
	// rejects others (*#*0##) for a few seconds.
	intercomProbesToRecover = 2
)

// intercomWatch follows whether the intercom firmware answers and tells the
// projector, so Home Assistant can show the doors, cameras and ringer as
// unavailable instead of failing when they are used. It knows a firmware
// reload the companion starts itself, and it notices a bus that refuses
// connections. While the intercom is unavailable it asks the bus until it
// answers again.
type intercomWatch struct {
	ctx    context.Context //nolint:containedctx // The probing goroutines outlive the call that starts them.
	probe  func(context.Context) error
	logger *slog.Logger

	interval, stopWait time.Duration

	mu     sync.Mutex
	reason core.IntercomUnavailableReason
	// sink applies the events; it is set once the publisher exists, see
	// setSink, and they are dropped before.
	sink func(core.Event)
}

func newIntercomWatch(ctx context.Context, probe func(context.Context) error, logger *slog.Logger) *intercomWatch {
	return &intercomWatch{
		ctx:      ctx,
		probe:    probe,
		logger:   logger.With("component", "intercom_watch"),
		interval: intercomProbeInterval,
		stopWait: intercomStopWait,
	}
}

// setSink sets where the events go.
func (w *intercomWatch) setSink(sink func(core.Event)) {
	w.mu.Lock()
	w.sink = sink
	w.mu.Unlock()
}

func (w *intercomWatch) apply(event core.Event) {
	w.mu.Lock()
	sink := w.sink
	w.mu.Unlock()

	if sink != nil {
		sink(event)
	}
}

// reloading is told just before the companion reloads the firmware.
func (w *intercomWatch) reloading() {
	w.markUnavailable(core.IntercomReloading, true)
}

// observe takes the result of a command sent to the bus: a refused
// connection means the firmware is down.
func (w *intercomWatch) observe(err error) {
	if err != nil && (errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)) {
		w.markUnavailable(core.IntercomUnreachable, false)
	}
}

// unavailable reports whether the intercom is unavailable now.
func (w *intercomWatch) unavailable() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.reason != ""
}

func (w *intercomWatch) markUnavailable(reason core.IntercomUnavailableReason, stopping bool) {
	w.mu.Lock()
	if w.reason != "" {
		w.mu.Unlock()
		return
	}

	w.reason = reason
	w.mu.Unlock()

	w.logger.Info("intercom unavailable", "reason", reason)
	w.apply(core.IntercomUnavailable{Reason: reason})

	go w.waitBack(stopping)
}

// waitBack asks the bus until it answers again. A reload first has to stop
// the firmware: until then the bus still answers, and that is not the end of
// the reload.
func (w *intercomWatch) waitBack(stopping bool) {
	started := time.Now()
	answers := 0

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
		}

		ctx, cancel := context.WithTimeout(w.ctx, intercomProbeTimeout)
		err := w.probe(ctx)

		cancel()

		if stopping {
			if err == nil && time.Since(started) < w.stopWait {
				continue
			}

			stopping = false
		}

		if err != nil {
			answers = 0
			continue
		}

		answers++
		if answers < intercomProbesToRecover {
			continue
		}

		w.mu.Lock()
		reason := w.reason
		w.reason = ""
		w.mu.Unlock()

		w.logger.Info("intercom available again", "reason", reason, "unavailable_for", time.Since(started).Round(time.Second))
		w.apply(core.IntercomAvailable{})

		return
	}
}

// watchedEntrypoints refuses an unlock at once while the intercom is
// unavailable, and tells the watch about the result of the others.
type watchedEntrypoints struct {
	unlock func(context.Context, core.EntrypointID) error
	watch  *intercomWatch
	// touched is told about every unlock asked, so a firmware reload waits.
	touched func()
}

func (e watchedEntrypoints) Unlock(ctx context.Context, id core.EntrypointID) error {
	e.touched()

	if e.watch.unavailable() {
		return core.ErrIntercomUnavailable
	}

	err := e.unlock(ctx, id)
	e.watch.observe(err)

	return err
}

// intercomActivity tells whether someone used the intercom lately: a firmware
// reload stops it for about 25 s, so it waits for intercomQuietTime without
// a ring, a call, a stream or an unlock. On 2026-11-22 a reload started 6 s
// after an unlock and refused the next ones.
type intercomActivity struct {
	quiet time.Duration
	now   func() time.Time

	mu   sync.Mutex
	last time.Time
}

// intercomQuietTime is how long the intercom must go unused before a
// firmware reload.
const intercomQuietTime = 30 * time.Second

func newIntercomActivity() *intercomActivity {
	return &intercomActivity{quiet: intercomQuietTime, now: time.Now}
}

func (a *intercomActivity) touch() {
	a.mu.Lock()
	a.last = a.now()
	a.mu.Unlock()
}

// busy reports the intercom in use now, per busyNow, or used within the quiet
// time; busy now counts as used.
func (a *intercomActivity) busy(busyNow bool) bool {
	if busyNow {
		a.touch()
		return true
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	return !a.last.IsZero() && a.now().Sub(a.last) < a.quiet
}

// intercomActivityEvent picks the events that are a use of the intercom.
func intercomActivityEvent(event core.Event) bool {
	switch event.(type) {
	case core.RingStarted, core.RingCleared, core.IncomingCallStarted, core.IncomingCallEnded, core.CallAnswered,
		core.CallDeclined, core.CallHungUp, core.PreviewStarted, core.PreviewStopped:
		return true
	default:
		return false
	}
}

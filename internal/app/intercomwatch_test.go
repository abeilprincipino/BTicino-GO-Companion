package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"syscall"
	"testing"
	"time"

	"bticino-go-companion/internal/core"
)

// scriptedProbe answers the watch's probes in turn, then repeats the last
// answer.
type scriptedProbe struct {
	mu      sync.Mutex
	answers []error
	asked   int
}

func (p *scriptedProbe) probe(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	i := min(p.asked, len(p.answers)-1)
	p.asked++

	return p.answers[i]
}

type eventLog struct {
	mu     sync.Mutex
	events []core.Event
}

func (l *eventLog) apply(event core.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.events = append(l.events, event)
}

func (l *eventLog) snapshot() []core.Event {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]core.Event(nil), l.events...)
}

var errRefused = fmt.Errorf("dial openwebnet: %w", syscall.ECONNREFUSED)

func newTestIntercomWatch(t *testing.T, probe *scriptedProbe) (*intercomWatch, *eventLog) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	log := &eventLog{}
	watch := newIntercomWatch(ctx, probe.probe, slog.New(slog.DiscardHandler))
	watch.interval = time.Millisecond
	watch.stopWait = time.Second
	watch.setSink(log.apply)

	return watch, log
}

func waitAvailable(t *testing.T, watch *intercomWatch) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for watch.unavailable() {
		if time.Now().After(deadline) {
			t.Fatal("the intercom never became available again")
		}

		time.Sleep(time.Millisecond)
	}
}

// A reload: the bus answers for a while, stops, and then answers again; the
// intercom is available once it answers twice in a row after stopping.
func TestIntercomWatchFollowsAFirmwareReload(t *testing.T) {
	probe := &scriptedProbe{answers: []error{nil, nil, errRefused, errRefused, nil, errRefused, nil, nil}}
	watch, log := newTestIntercomWatch(t, probe)

	watch.reloading()

	if !watch.unavailable() {
		t.Fatal("the intercom is available while it reloads")
	}

	waitAvailable(t, watch)

	events := log.snapshot()
	if len(events) != 2 {
		t.Fatalf("events = %#v, want unavailable then available", events)
	}

	if unavailable, ok := events[0].(core.IntercomUnavailable); !ok || unavailable.Reason != core.IntercomReloading {
		t.Fatalf("first event = %#v, want IntercomUnavailable for the reload", events[0])
	}

	if _, ok := events[1].(core.IntercomAvailable); !ok {
		t.Fatalf("second event = %#v, want IntercomAvailable", events[1])
	}

	if probe.asked != 8 {
		t.Fatalf("probes = %d, want the 8 answers used", probe.asked)
	}
}

// A reload whose stop the watch never sees still ends, after stopWait.
func TestIntercomWatchEndsAReloadItNeverSawStop(t *testing.T) {
	probe := &scriptedProbe{answers: []error{nil}}
	watch, log := newTestIntercomWatch(t, probe)
	watch.stopWait = 20 * time.Millisecond

	watch.reloading()
	waitAvailable(t, watch)

	if events := log.snapshot(); len(events) != 2 {
		t.Fatalf("events = %#v, want unavailable then available", events)
	}
}

func TestIntercomWatchNoticesARefusedConnection(t *testing.T) {
	probe := &scriptedProbe{answers: []error{errRefused, nil}}
	watch, log := newTestIntercomWatch(t, probe)

	watch.observe(nil)
	watch.observe(errors.New("openwebnet unexpected reply"))

	if watch.unavailable() {
		t.Fatal("an unexpected reply made the intercom unavailable")
	}

	watch.observe(errRefused)
	watch.observe(errRefused)

	if events := log.snapshot(); len(events) != 1 {
		t.Fatalf("events = %#v, want one IntercomUnavailable however many refusals", events)
	}

	waitAvailable(t, watch)

	if unavailable, ok := log.snapshot()[0].(core.IntercomUnavailable); !ok || unavailable.Reason != core.IntercomUnreachable {
		t.Fatalf("first event = %#v, want IntercomUnavailable, unreachable", log.snapshot()[0])
	}
}

func TestWatchedEntrypointsRefusesAnUnlockWhileUnavailable(t *testing.T) {
	probe := &scriptedProbe{answers: []error{errRefused}}
	watch, _ := newTestIntercomWatch(t, probe)

	unlocks, touches := 0, 0
	entrypoints := watchedEntrypoints{
		unlock: func(context.Context, core.EntrypointID) error {
			unlocks++
			return errRefused
		},
		watch:   watch,
		touched: func() { touches++ },
	}

	if err := entrypoints.Unlock(context.Background(), "portal6"); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("first unlock error = %v, want the refusal", err)
	}

	if err := entrypoints.Unlock(context.Background(), "portal6"); !errors.Is(err, core.ErrIntercomUnavailable) {
		t.Fatalf("second unlock error = %v, want ErrIntercomUnavailable", err)
	}

	if unlocks != 1 || touches != 2 {
		t.Fatalf("unlocks = %d, touches = %d; want 1 sent and 2 counted as use", unlocks, touches)
	}
}

func TestIntercomActivityKeepsTheIntercomBusyForTheQuietTime(t *testing.T) {
	now := time.Now()
	activity := newIntercomActivity()
	activity.now = func() time.Time { return now }

	if activity.busy(false) {
		t.Fatal("busy before any use")
	}

	activity.touch()
	now = now.Add(intercomQuietTime - time.Second)

	if !activity.busy(false) {
		t.Fatal("not busy within the quiet time of an unlock")
	}

	now = now.Add(2 * time.Second)

	if activity.busy(false) {
		t.Fatal("busy after the quiet time")
	}

	if !activity.busy(true) {
		t.Fatal("not busy while in use")
	}

	now = now.Add(time.Second)

	if !activity.busy(false) {
		t.Fatal("not busy just after a use")
	}
}

package ringersync

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeAudio struct {
	mu    sync.Mutex
	muted bool
	err   error
}

func (a *fakeAudio) Mute(context.Context) error   { return a.set(true) }
func (a *fakeAudio) Unmute(context.Context) error { return a.set(false) }

func (a *fakeAudio) set(muted bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.err != nil {
		return a.err
	}

	a.muted = muted

	return nil
}

func (a *fakeAudio) isMuted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.muted
}

type harness struct {
	sync    *Sync
	audio   *fakeAudio
	reloads chan struct{}
	busy    atomic.Bool
}

func newHarness(t *testing.T, muted bool) *harness {
	t.Helper()

	h := &harness{audio: &fakeAudio{muted: muted}, reloads: make(chan struct{}, 8)}
	h.sync = New(Config{
		Audio: h.audio,
		Muted: h.audio.isMuted,
		Busy:  h.busy.Load,
		Reload: func(context.Context) error {
			h.reloads <- struct{}{}
			return nil
		},
		Delay:  20 * time.Millisecond,
		Poll:   5 * time.Millisecond,
		Logger: slog.New(slog.DiscardHandler),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		h.sync.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("Run did not stop after cancellation")
		}
	})

	return h
}

func (h *harness) expectReloads(t *testing.T, want int) {
	t.Helper()

	got := 0
	deadline := time.After(200 * time.Millisecond)

	for {
		select {
		case <-h.reloads:
			got++
		case <-deadline:
			if got != want {
				t.Fatalf("reloads = %d, want %d", got, want)
			}

			return
		}
	}
}

// The display follows a mute sent over OpenWebNet but not an unmute (C100X,
// 2026-11-05), so an unmute from the companion reloads the firmware.
func TestUnmuteOfAMutedRingerReloadsOnce(t *testing.T) {
	h := newHarness(t, true)

	if err := h.sync.Unmute(context.Background()); err != nil {
		t.Fatal(err)
	}

	h.expectReloads(t, 1)
}

func TestUnmuteOfARingerAlreadyOnDoesNotReload(t *testing.T) {
	h := newHarness(t, false)

	if err := h.sync.Unmute(context.Background()); err != nil {
		t.Fatal(err)
	}

	h.expectReloads(t, 0)
}

// The display follows a mute by itself: a mute before the reload makes it
// pointless.
func TestMuteBeforeTheReloadCancelsIt(t *testing.T) {
	h := newHarness(t, true)

	if err := h.sync.Unmute(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := h.sync.Mute(context.Background()); err != nil {
		t.Fatal(err)
	}

	h.expectReloads(t, 0)
}

func TestTogglingCoalescesIntoOneReload(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	for _, step := range []func(context.Context) error{h.sync.Unmute, h.sync.Mute, h.sync.Unmute, h.sync.Mute, h.sync.Unmute} {
		if err := step(ctx); err != nil {
			t.Fatal(err)
		}
	}

	h.expectReloads(t, 1)
}

// A reload stops the firmware for about 25 s: never during a ring, a call or
// a stream.
func TestReloadWaitsUntilTheIntercomIsIdle(t *testing.T) {
	h := newHarness(t, true)
	h.busy.Store(true)

	if err := h.sync.Unmute(context.Background()); err != nil {
		t.Fatal(err)
	}

	h.expectReloads(t, 0)
	h.busy.Store(false)
	h.expectReloads(t, 1)
}

func TestFailedUnmuteDoesNotReload(t *testing.T) {
	h := newHarness(t, true)
	h.audio.err = errors.New("nack")

	if err := h.sync.Unmute(context.Background()); err == nil {
		t.Fatal("Unmute() error = nil, want the command's error")
	}

	h.expectReloads(t, 0)
}

package signaling

import (
	"context"
	"testing"
	"time"
)

func TestResolveInviteTargetUsesProfileEndpointAndDomain(t *testing.T) {
	t.Parallel()

	target, err := resolveInviteTarget("c300x@127.0.0.1", "example.local")
	if err != nil {
		t.Fatalf("resolveInviteTarget() error = %v", err)
	}
	if target.URI.User != "c300x" || target.URI.Host != "example.local" {
		t.Fatalf("target URI = %s, want sip:c300x@example.local", target.URI.String())
	}
	if target.destination != "127.0.0.1:5060" {
		t.Fatalf("target destination = %q, want 127.0.0.1:5060", target.destination)
	}
}

func TestNewStreamDialerRequiresTarget(t *testing.T) {
	t.Parallel()

	_, err := NewStreamDialer(StreamDialerConfig{})
	if err != ErrStreamTargetUnset {
		t.Fatalf("NewStreamDialer() error = %v, want %v", err, ErrStreamTargetUnset)
	}
}

func TestStreamDialerSetRemoteDialogEndedReplacesActiveStreamCallback(t *testing.T) {
	dialer := &streamDialer{}
	first, second := 0, 0
	dialer.SetRemoteDialogEnded(func() { first++ })
	dialer.SetRemoteDialogEnded(func() { second++ })

	dialer.callbackMu.RLock()
	callback := dialer.remoteDialogEnded
	dialer.callbackMu.RUnlock()
	callback()

	if first != 0 || second != 1 {
		t.Fatalf("callback counts = first:%d second:%d, want first:0 second:1", first, second)
	}
}

func TestRegistrationLoopRefreshesAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := make(chan struct{}, 2)
	done := make(chan struct{})
	go func() {
		registrationLoop(ctx, time.Millisecond, time.Millisecond, time.Second, func(context.Context) error {
			select {
			case calls <- struct{}{}:
			default:
			}
			return nil
		})
		close(done)
	}()

	for range 2 {
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatal("registration was not refreshed")
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("registration loop did not stop after cancellation")
	}
}

func TestWaitForDialogEnd(t *testing.T) {
	t.Run("dialog ended", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		if err := waitForDialogEnd(context.Background(), done); err != nil {
			t.Fatalf("waitForDialogEnd() error = %v", err)
		}
	})

	t.Run("context canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := waitForDialogEnd(ctx, make(chan struct{})); err != context.Canceled {
			t.Fatalf("waitForDialogEnd() error = %v, want context.Canceled", err)
		}
	})
}

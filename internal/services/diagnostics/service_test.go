package diagnostics

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"bticino-go-companion/internal/system"
)

func TestRefreshSuccess(t *testing.T) {
	expected := 67
	svc := NewForTest(time.Second, log.New(io.Discard, "", 0), func() (system.NetworkSnapshot, bool) {
		return system.NetworkSnapshot{
			IP:       "192.0.2.10",
			Netmask:  "255.255.255.0",
			MAC:      "00:11:22:33:44:55",
			WiFiRSSI: &expected,
		}, true
	})

	svc.Refresh()
	snap := svc.NetworkSnapshot()

	if snap.IP != "192.0.2.10" {
		t.Fatalf("unexpected ip: %q", snap.IP)
	}
	if snap.Netmask != "255.255.255.0" {
		t.Fatalf("unexpected netmask: %q", snap.Netmask)
	}
	if snap.MAC != "00:11:22:33:44:55" {
		t.Fatalf("unexpected mac: %q", snap.MAC)
	}
	if snap.WiFiStrength == nil || *snap.WiFiStrength != expected {
		t.Fatalf("unexpected wifi strength: %#v", snap.WiFiStrength)
	}
	if snap.UpdatedAt == nil {
		t.Fatal("expected updated_at to be set")
	}
	if snap.Stale {
		t.Fatal("expected stale=false")
	}
}

func TestRefreshFailureMarksStaleAndKeepsLastGood(t *testing.T) {
	count := 0
	value := 42
	svc := NewForTest(time.Second, log.New(io.Discard, "", 0), func() (system.NetworkSnapshot, bool) {
		count++
		if count == 1 {
			return system.NetworkSnapshot{
				IP:       "192.0.2.20",
				Netmask:  "255.255.255.0",
				MAC:      "00:11:22:aa:bb:cc",
				WiFiRSSI: &value,
			}, true
		}
		return system.NetworkSnapshot{}, false
	})

	svc.Refresh()
	first := svc.NetworkSnapshot()
	if first.Stale {
		t.Fatal("expected first snapshot fresh")
	}

	svc.Refresh()
	second := svc.NetworkSnapshot()
	if !second.Stale {
		t.Fatal("expected stale flag after detector failure")
	}
	if second.IP != first.IP || second.MAC != first.MAC {
		t.Fatalf("expected last good snapshot retained, got first=%+v second=%+v", first, second)
	}
}

func TestStartRunsRefreshLoop(t *testing.T) {
	value := 10
	svc := NewForTest(20*time.Millisecond, log.New(io.Discard, "", 0), func() (system.NetworkSnapshot, bool) {
		value++
		next := value
		return system.NetworkSnapshot{WiFiRSSI: &next}, true
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Start(ctx)
	}()

	time.Sleep(70 * time.Millisecond)
	cancel()
	<-done

	snap := svc.NetworkSnapshot()
	if snap.WiFiStrength == nil {
		t.Fatal("expected wifi strength from loop refresh")
	}
	if *snap.WiFiStrength <= 10 {
		t.Fatalf("expected loop to refresh more than once, got %d", *snap.WiFiStrength)
	}
}

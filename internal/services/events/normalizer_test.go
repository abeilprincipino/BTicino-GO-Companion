package events

import (
	"testing"
	"time"

	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/domain/event"
)

func TestNormalizerAssignsEntrypointFromDevAddrPayload(t *testing.T) {
	n := NewNormalizer([]entrypoint.Model{{ID: "gate2", DevAddr: "21"}})
	ev := n.Normalize(event.Envelope{
		Type:    event.TypeUnlockPulseStart,
		Source:  event.SourceOpenWebNet,
		Payload: map[string]any{"devaddr": "21"},
	})
	if ev.EntrypointID != "gate2" {
		t.Fatalf("expected gate2, got %q", ev.EntrypointID)
	}
}

func TestNormalizerAssignsEntrypointFromRawFrame(t *testing.T) {
	n := NewNormalizer([]entrypoint.Model{{ID: "main", DevAddr: "20"}})
	ev := n.Normalize(event.Envelope{
		Type:   event.TypeRingStarted,
		Source: event.SourceOpenWebNet,
		Raw:    "*8*1#1#4#10*20##",
	})
	if ev.EntrypointID != "main" {
		t.Fatalf("expected main, got %q", ev.EntrypointID)
	}
	if ev.Payload["devaddr"] != "20" {
		t.Fatalf("expected payload devaddr 20, got %#v", ev.Payload["devaddr"])
	}
}

func TestNormalizerAssignsFloorEntrypoint(t *testing.T) {
	n := NewNormalizer(nil)
	ev := n.Normalize(event.Envelope{Type: event.TypeRingFloorStarted, Source: event.SourceOpenWebNet})
	if ev.EntrypointID != "floor" {
		t.Fatalf("expected floor, got %q", ev.EntrypointID)
	}
}

func TestNormalizerFillsDefaults(t *testing.T) {
	n := NewNormalizer(nil)
	ev := n.Normalize(event.Envelope{Type: "x", Raw: "*1*2##"})
	if ev.Source != event.SourceSystem {
		t.Fatalf("expected default source system, got %q", ev.Source)
	}
	if ev.TS.IsZero() {
		t.Fatal("expected timestamp set")
	}
	if _, ok := ev.Payload["raw"]; !ok {
		t.Fatal("expected raw copied to payload")
	}
	if time.Since(ev.TS) > 2*time.Second {
		t.Fatalf("unexpected timestamp age: %v", time.Since(ev.TS))
	}
}

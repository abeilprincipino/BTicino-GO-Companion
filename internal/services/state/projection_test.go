package state

import (
	"testing"
	"time"

	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/domain/event"
)

func TestProjectorApply(t *testing.T) {
	p := NewProjector([]entrypoint.Model{{ID: "main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	now := time.Now()
	p.Apply(event.Envelope{Type: event.TypeRingStarted, TS: now, EntrypointID: "main"})
	s := p.Snapshot()
	if !s.Ringing || s.CallState != CallStateRinging {
		t.Fatalf("unexpected ring state: %+v", s)
	}
	p.Apply(event.Envelope{Type: event.TypeStreamStarted, TS: now, EntrypointID: "main"})
	s = p.Snapshot()
	if !s.StreamActive {
		t.Fatalf("stream should be active: %+v", s)
	}
	if s.ActiveEntrypoint != "main" {
		t.Fatalf("expected active entrypoint main, got %q", s.ActiveEntrypoint)
	}
	p.Apply(event.Envelope{Type: event.TypeRingEnded, TS: now})
	p.Apply(event.Envelope{Type: event.TypeStreamStopped, TS: now})
	s = p.Snapshot()
	if s.StreamActive || s.CallState != CallStateIdle {
		t.Fatalf("stream stop not applied: %+v", s)
	}
	if s.ActiveEntrypoint != "" {
		t.Fatalf("expected cleared active entrypoint, got %q", s.ActiveEntrypoint)
	}
}

func TestProjectorHeartbeatDoesNotMutateState(t *testing.T) {
	p := NewProjector([]entrypoint.Model{{ID: "main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	now := time.Now()
	p.Apply(event.Envelope{Type: event.TypeRingStarted, TS: now, EntrypointID: "main"})
	before := p.Snapshot()

	p.Apply(event.Envelope{Type: event.TypeHeartbeat, TS: now.Add(time.Second)})
	after := p.Snapshot()

	if before.LastEventType != after.LastEventType {
		t.Fatalf("heartbeat should not update last event type: before=%s after=%s", before.LastEventType, after.LastEventType)
	}
	if before.CallState != after.CallState || before.ActiveEntrypoint != after.ActiveEntrypoint || before.StreamActive != after.StreamActive || before.Ringing != after.Ringing {
		t.Fatalf("heartbeat should not mutate state: before=%+v after=%+v", before, after)
	}
}

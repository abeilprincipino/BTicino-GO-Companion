package state

import (
	"testing"
	"time"

	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/domain/event"
)

func TestProjectorApply(t *testing.T) {
	p := NewProjector([]entrypoint.Model{{ID: "main", DevAddr: "20", HasStream: true, HasUnlock: true, HasRing: true}})
	now := time.Now().UTC()
	p.Apply(event.Envelope{Type: "ring.started", TS: now})
	s := p.Snapshot()
	if !s.Ringing || s.CallState != "ringing" {
		t.Fatalf("unexpected ring state: %+v", s)
	}
	p.Apply(event.Envelope{Type: "stream.started", TS: now})
	s = p.Snapshot()
	if !s.StreamActive {
		t.Fatalf("stream should be active: %+v", s)
	}
	p.Apply(event.Envelope{Type: "stream.stopped", TS: now})
	s = p.Snapshot()
	if s.StreamActive || s.CallState != "idle" {
		t.Fatalf("stream stop not applied: %+v", s)
	}
}

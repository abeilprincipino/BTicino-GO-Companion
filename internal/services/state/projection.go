package state

import (
	"sync"
	"time"

	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/domain/event"
)

type Snapshot struct {
	BootTime         time.Time          `json:"boot_time"`
	CallState        string             `json:"call_state"`
	StreamActive     bool               `json:"stream_active"`
	ActiveEntrypoint string             `json:"active_entrypoint,omitempty"`
	Ringing          bool               `json:"ringing"`
	FloorRinging     bool               `json:"floor_ringing"`
	LastEventType    string             `json:"last_event_type,omitempty"`
	LastEventTS      *time.Time         `json:"last_event_ts,omitempty"`
	Entrypoints      []entrypoint.Model `json:"entrypoints"`
}

type Projector struct {
	mu       sync.RWMutex
	nextID   uint64
	snapshot Snapshot
}

func NewProjector(entrypoints []entrypoint.Model) *Projector {
	return &Projector{
		nextID: 1,
		snapshot: Snapshot{
			BootTime:     time.Now(),
			CallState:    CallStateIdle,
			Entrypoints:  entrypoints,
			StreamActive: false,
			Ringing:      false,
		},
	}
}

func (p *Projector) Apply(ev event.Envelope) event.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()

	ev.ID = p.nextID
	p.nextID++

	s := &p.snapshot
	s.LastEventType = ev.Type
	ts := ev.TS
	s.LastEventTS = &ts

	applyTransition(s, ev)

	return ev
}

func (p *Projector) Snapshot() Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	copy := p.snapshot
	copy.Entrypoints = append([]entrypoint.Model(nil), p.snapshot.Entrypoints...)
	return copy
}

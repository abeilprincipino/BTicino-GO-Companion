package events

import (
	"context"
	"sync"

	"bticino-go-companion/internal/domain/event"
)

type Broker struct {
	mu          sync.RWMutex
	maxRetained int
	events      []event.Envelope
	subs        map[chan event.Envelope]struct{}
}

func New(maxRetained int) *Broker {
	if maxRetained <= 0 {
		maxRetained = 512
	}
	return &Broker{
		maxRetained: maxRetained,
		events:      make([]event.Envelope, 0, maxRetained),
		subs:        map[chan event.Envelope]struct{}{},
	}
}

func (b *Broker) Publish(ev event.Envelope) {
	b.mu.Lock()
	b.events = append(b.events, ev)
	if len(b.events) > b.maxRetained {
		offset := len(b.events) - b.maxRetained
		copy(b.events, b.events[offset:])
		b.events = b.events[:b.maxRetained]
	}
	snapshotSubs := make([]chan event.Envelope, 0, len(b.subs))
	for ch := range b.subs {
		snapshotSubs = append(snapshotSubs, ch)
	}
	b.mu.Unlock()

	for _, ch := range snapshotSubs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (b *Broker) ReplaySince(lastID uint64) []event.Envelope {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]event.Envelope, 0)
	for _, ev := range b.events {
		if ev.ID > lastID {
			out = append(out, ev)
		}
	}
	return out
}

func (b *Broker) Subscribe(ctx context.Context) <-chan event.Envelope {
	ch := make(chan event.Envelope, 32)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, ch)
		close(ch)
		b.mu.Unlock()
	}()
	return ch
}

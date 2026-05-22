package trace

import (
	"context"
	"sync"
	"time"
)

type Record struct {
	ID               uint64            `json:"id"`
	TS               time.Time         `json:"ts"`
	Direction        string            `json:"direction,omitempty"`
	Transport        string            `json:"transport,omitempty"`
	System           string            `json:"system,omitempty"`
	Frame            string            `json:"frame,omitempty"`
	Mapped           bool              `json:"mapped"`
	DecodedEventType []string          `json:"decoded_event_types,omitempty"`
	Meta             map[string]string `json:"meta,omitempty"`
}

type Broker struct {
	mu          sync.RWMutex
	maxRetained int
	nextID      uint64
	records     []Record
	subs        map[chan Record]struct{}
}

func New(maxRetained int) *Broker {
	if maxRetained <= 0 {
		maxRetained = 512
	}
	return &Broker{
		maxRetained: maxRetained,
		nextID:      1,
		records:     make([]Record, 0, maxRetained),
		subs:        map[chan Record]struct{}{},
	}
}

func (b *Broker) Publish(rec Record) {
	b.mu.Lock()
	if rec.TS.IsZero() {
		rec.TS = time.Now()
	}
	if rec.ID == 0 {
		rec.ID = b.nextID
		b.nextID++
	}
	b.records = append(b.records, rec)
	if len(b.records) > b.maxRetained {
		offset := len(b.records) - b.maxRetained
		copy(b.records, b.records[offset:])
		b.records = b.records[:b.maxRetained]
	}
	b.mu.Unlock()

	b.mu.RLock()
	for ch := range b.subs {
		select {
		case ch <- rec:
		default:
		}
	}
	b.mu.RUnlock()
}

func (b *Broker) ReplaySince(lastID uint64) []Record {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Record, 0)
	for _, rec := range b.records {
		if rec.ID > lastID {
			out = append(out, rec)
		}
	}
	return out
}

func (b *Broker) Subscribe(ctx context.Context) <-chan Record {
	ch := make(chan Record, 32)
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

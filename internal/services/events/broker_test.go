package events

import (
	"context"
	"testing"
	"time"

	"bticino-go-companion/internal/domain/event"
)

func TestReplaySince(t *testing.T) {
	b := New(10)
	b.Publish(event.Envelope{ID: 1, Type: "a"})
	b.Publish(event.Envelope{ID: 2, Type: "b"})
	b.Publish(event.Envelope{ID: 3, Type: "c"})

	replay := b.ReplaySince(1)
	if len(replay) != 2 || replay[0].ID != 2 || replay[1].ID != 3 {
		t.Fatalf("unexpected replay: %+v", replay)
	}
}

func TestSubscribeReceivesPublishedEvent(t *testing.T) {
	b := New(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)

	b.Publish(event.Envelope{ID: 4, Type: "x"})

	select {
	case ev := <-ch:
		if ev.ID != 4 {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestPublishAssignsMonotonicIDs(t *testing.T) {
	b := New(10)
	first := b.Publish(event.Envelope{Type: "x"})
	second := b.Publish(event.Envelope{Type: "y"})
	if first.ID == 0 || second.ID == 0 {
		t.Fatalf("expected broker-assigned ids, got first=%d second=%d", first.ID, second.ID)
	}
	if second.ID != first.ID+1 {
		t.Fatalf("expected monotonic ids, got first=%d second=%d", first.ID, second.ID)
	}
}

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

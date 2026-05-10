package trace

import (
	"context"
	"testing"
)

func TestBrokerReplaySince(t *testing.T) {
	b := New(8)
	b.Publish(Record{Frame: "a"})
	b.Publish(Record{Frame: "b"})
	out := b.ReplaySince(1)
	if len(out) != 1 || out[0].Frame != "b" {
		t.Fatalf("unexpected replay: %+v", out)
	}
}

func TestBrokerSubscribe(t *testing.T) {
	b := New(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := b.Subscribe(ctx)
	b.Publish(Record{Frame: "x"})
	rec := <-sub
	if rec.Frame != "x" {
		t.Fatalf("unexpected frame: %+v", rec)
	}
}

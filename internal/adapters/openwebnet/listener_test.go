package openwebnet

import (
	"context"
	"testing"

	"bticino-go-companion/internal/domain/event"
	openwebnetproto "bticino-go-companion/internal/protocol/openwebnet"
)

func TestNewListenerDefaults(t *testing.T) {
	l := NewListener("239.255.76.67", 7667, 0)
	if l == nil {
		t.Fatal("expected listener instance")
	}
	if l.buffer != 65535 {
		t.Fatalf("expected default buffer 65535, got %d", l.buffer)
	}
	if l.parser == nil || l.mapper == nil {
		t.Fatalf("expected parser/mapper initialized: %+v", l)
	}
}

func TestListenerSetTraceSinkAndInvalidGroup(t *testing.T) {
	l := NewListener("invalid-group", 7667, 4096)
	called := false
	l.SetTraceSink(func(openwebnetproto.Message, []event.Envelope) {
		called = true
	})
	if l.traceSink == nil {
		t.Fatal("expected trace sink set")
	}

	err := l.Run(context.Background(), func(event.Envelope) {})
	if err == nil {
		t.Fatal("expected invalid group error")
	}
	if called {
		t.Fatal("trace sink should not be called on invalid group")
	}
}

package control

import (
	"context"
	"testing"

	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/domain/event"
)

type unlockStub struct {
	devAddr string
	err     error
}

func (u *unlockStub) Unlock(_ context.Context, devAddr string) error {
	u.devAddr = devAddr
	return u.err
}

type streamStub struct {
	startDevAddr string
	startErr     error
	stopErr      error
}

func (s *streamStub) StreamStart(_ context.Context, devAddr string) error {
	s.startDevAddr = devAddr
	return s.startErr
}

func (s *streamStub) StreamStop(_ context.Context) error {
	return s.stopErr
}

func TestServiceUnlockEntrypoint(t *testing.T) {
	unlock := &unlockStub{}
	stream := &streamStub{}
	var events []event.Envelope
	svc := New([]entrypoint.Model{{ID: "main", DevAddr: "21", HasUnlock: true, HasStream: true}}, stream, unlock, func(ev event.Envelope) {
		events = append(events, ev)
	})

	if err := svc.UnlockEntrypoint(context.Background(), "main"); err != nil {
		t.Fatalf("unlock failed: %v", err)
	}
	if unlock.devAddr != "21" {
		t.Fatalf("unexpected devaddr: %s", unlock.devAddr)
	}
	if len(events) != 1 || events[0].Type != event.TypeUnlockTriggered || events[0].EntrypointID != "main" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestServiceStreamStartUsesEntrypointDevAddr(t *testing.T) {
	unlock := &unlockStub{}
	stream := &streamStub{}
	svc := New([]entrypoint.Model{{ID: "gate2", DevAddr: "22", HasStream: true, HasUnlock: true}}, stream, unlock, nil)

	if err := svc.StartEntrypointStream(context.Background(), "gate2"); err != nil {
		t.Fatalf("stream start failed: %v", err)
	}
	if stream.startDevAddr != "22" {
		t.Fatalf("unexpected stream devaddr: %s", stream.startDevAddr)
	}
}

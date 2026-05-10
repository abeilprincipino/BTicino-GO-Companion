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
	startEntrypID string
	startDevAddr  string
	startErr      error
	stopEntrypID  string
	stopErr       error
}

func (s *streamStub) StartForEntrypoint(_ context.Context, entrypointID string, devAddr string) error {
	s.startEntrypID = entrypointID
	s.startDevAddr = devAddr
	return s.startErr
}

func (s *streamStub) StopForEntrypoint(_ context.Context, entrypointID string) error {
	s.stopEntrypID = entrypointID
	return s.stopErr
}

type callStub struct {
	answerCalls int
	hangupCalls int
	answerErr   error
	hangupErr   error
}

func (c *callStub) Answer(context.Context) error {
	c.answerCalls++
	return c.answerErr
}

func (c *callStub) Hangup(context.Context) error {
	c.hangupCalls++
	return c.hangupErr
}

func TestServiceUnlockEntrypoint(t *testing.T) {
	unlock := &unlockStub{}
	stream := &streamStub{}
	call := &callStub{}
	var events []event.Envelope
	svc := New([]entrypoint.Model{{ID: "main", DevAddr: "21", HasUnlock: true, HasStream: true}}, stream, unlock, call, func(ev event.Envelope) {
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
	call := &callStub{}
	svc := New([]entrypoint.Model{{ID: "gate2", DevAddr: "22", HasStream: true, HasUnlock: true}}, stream, unlock, call, nil)

	if err := svc.StartEntrypointStream(context.Background(), "gate2"); err != nil {
		t.Fatalf("stream start failed: %v", err)
	}
	if stream.startDevAddr != "22" {
		t.Fatalf("unexpected stream devaddr: %s", stream.startDevAddr)
	}
	if stream.startEntrypID != "gate2" {
		t.Fatalf("unexpected stream entrypoint id: %s", stream.startEntrypID)
	}
}

func TestServiceCallAnswerHangup(t *testing.T) {
	unlock := &unlockStub{}
	stream := &streamStub{}
	call := &callStub{}
	var emitted []event.Envelope
	svc := New([]entrypoint.Model{{ID: "main", DevAddr: "20", HasStream: true, HasUnlock: true}}, stream, unlock, call, func(ev event.Envelope) {
		emitted = append(emitted, ev)
	})

	if err := svc.AnswerCall(context.Background()); err != nil {
		t.Fatalf("answer failed: %v", err)
	}
	if err := svc.HangupCall(context.Background()); err != nil {
		t.Fatalf("hangup failed: %v", err)
	}
	if call.answerCalls != 1 || call.hangupCalls != 1 {
		t.Fatalf("unexpected call driver invocations answer=%d hangup=%d", call.answerCalls, call.hangupCalls)
	}
	if len(emitted) != 2 || emitted[0].Type != event.TypeCallAnswered || emitted[1].Type != event.TypeCallEnded {
		t.Fatalf("unexpected emitted events: %+v", emitted)
	}
}

package control

import (
	"context"
	"errors"
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

type audioStub struct {
	muteCalls   int
	unmuteCalls int
	muteErr     error
	unmuteErr   error
}

type voicemailStub struct {
	enableCalls  int
	disableCalls int
	enableErr    error
	disableErr   error
}

func (a *audioStub) Mute(context.Context) error {
	a.muteCalls++
	return a.muteErr
}

func (a *audioStub) Unmute(context.Context) error {
	a.unmuteCalls++
	return a.unmuteErr
}

func (v *voicemailStub) VoicemailEnable(context.Context) error {
	v.enableCalls++
	return v.enableErr
}

func (v *voicemailStub) VoicemailDisable(context.Context) error {
	v.disableCalls++
	return v.disableErr
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
	audio := &audioStub{}
	voicemail := &voicemailStub{}
	var events []event.Envelope
	svc := New([]entrypoint.Model{{ID: "main", DevAddr: "21", HasUnlock: true, HasStream: true}}, stream, unlock, call, audio, voicemail, func(ev event.Envelope) {
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
	audio := &audioStub{}
	voicemail := &voicemailStub{}
	svc := New([]entrypoint.Model{{ID: "gate2", DevAddr: "22", HasStream: true, HasUnlock: true}}, stream, unlock, call, audio, voicemail, nil)

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
	audio := &audioStub{}
	voicemail := &voicemailStub{}
	var emitted []event.Envelope
	svc := New([]entrypoint.Model{{ID: "main", DevAddr: "20", HasStream: true, HasUnlock: true}}, stream, unlock, call, audio, voicemail, func(ev event.Envelope) {
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

func TestServiceAudioMuteUnmute(t *testing.T) {
	unlock := &unlockStub{}
	stream := &streamStub{}
	call := &callStub{}
	audio := &audioStub{}
	voicemail := &voicemailStub{}
	var emitted []event.Envelope
	svc := New([]entrypoint.Model{{ID: "main", DevAddr: "20", HasStream: true, HasUnlock: true}}, stream, unlock, call, audio, voicemail, func(ev event.Envelope) {
		emitted = append(emitted, ev)
	})

	if err := svc.MuteAudio(context.Background()); err != nil {
		t.Fatalf("mute failed: %v", err)
	}
	if err := svc.UnmuteAudio(context.Background()); err != nil {
		t.Fatalf("unmute failed: %v", err)
	}
	if audio.muteCalls != 1 || audio.unmuteCalls != 1 {
		t.Fatalf("unexpected audio driver invocations mute=%d unmute=%d", audio.muteCalls, audio.unmuteCalls)
	}
	if len(emitted) != 2 || emitted[0].Type != event.TypeAudioMuted || emitted[1].Type != event.TypeAudioUnmuted {
		t.Fatalf("unexpected emitted events: %+v", emitted)
	}
}

func TestServiceVoicemailEnableDisable(t *testing.T) {
	unlock := &unlockStub{}
	stream := &streamStub{}
	call := &callStub{}
	audio := &audioStub{}
	voicemail := &voicemailStub{}
	var emitted []event.Envelope
	svc := New([]entrypoint.Model{{ID: "main", DevAddr: "20", HasStream: true, HasUnlock: true}}, stream, unlock, call, audio, voicemail, func(ev event.Envelope) {
		emitted = append(emitted, ev)
	})

	if err := svc.EnableVoicemail(context.Background()); err != nil {
		t.Fatalf("voicemail enable failed: %v", err)
	}
	if err := svc.DisableVoicemail(context.Background()); err != nil {
		t.Fatalf("voicemail disable failed: %v", err)
	}
	if voicemail.enableCalls != 1 || voicemail.disableCalls != 1 {
		t.Fatalf("unexpected voicemail driver invocations enable=%d disable=%d", voicemail.enableCalls, voicemail.disableCalls)
	}
	if len(emitted) != 2 || emitted[0].Type != event.TypeVoicemailEnabled || emitted[1].Type != event.TypeVoicemailDisabled {
		t.Fatalf("unexpected emitted events: %+v", emitted)
	}
}

func TestServiceErrorBranches(t *testing.T) {
	svc := New([]entrypoint.Model{{ID: "main", DevAddr: "20", HasStream: false, HasUnlock: false}}, &streamStub{}, &unlockStub{}, nil, nil, nil, nil)

	if err := svc.UnlockEntrypoint(context.Background(), "missing"); !errors.Is(err, ErrEntrypointNotFound) {
		t.Fatalf("expected ErrEntrypointNotFound, got %v", err)
	}
	if err := svc.UnlockEntrypoint(context.Background(), "main"); !errors.Is(err, ErrCapabilityNotEnabled) {
		t.Fatalf("expected ErrCapabilityNotEnabled for unlock, got %v", err)
	}
	if err := svc.StartEntrypointStream(context.Background(), "main"); !errors.Is(err, ErrCapabilityNotEnabled) {
		t.Fatalf("expected ErrCapabilityNotEnabled for stream start, got %v", err)
	}
	if err := svc.StopEntrypointStream(context.Background(), "main"); !errors.Is(err, ErrCapabilityNotEnabled) {
		t.Fatalf("expected ErrCapabilityNotEnabled for stream stop, got %v", err)
	}
	if err := svc.AnswerCall(context.Background()); !errors.Is(err, ErrNoIncomingCall) {
		t.Fatalf("expected ErrNoIncomingCall, got %v", err)
	}
	if err := svc.HangupCall(context.Background()); !errors.Is(err, ErrNoActiveCall) {
		t.Fatalf("expected ErrNoActiveCall, got %v", err)
	}
	if err := svc.MuteAudio(context.Background()); !errors.Is(err, ErrAudioControlDisabled) {
		t.Fatalf("expected ErrAudioControlDisabled, got %v", err)
	}
	if err := svc.EnableVoicemail(context.Background()); !errors.Is(err, ErrVoicemailUnavailable) {
		t.Fatalf("expected ErrVoicemailUnavailable, got %v", err)
	}
}

func TestMapCallError(t *testing.T) {
	if err := mapCallError(nil); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if err := mapCallError(errors.New("No incoming event")); !errors.Is(err, ErrNoIncomingCall) {
		t.Fatalf("expected ErrNoIncomingCall, got %v", err)
	}
	if err := mapCallError(errors.New("no active call right now")); !errors.Is(err, ErrNoActiveCall) {
		t.Fatalf("expected ErrNoActiveCall, got %v", err)
	}
	other := errors.New("transport failure")
	if err := mapCallError(other); !errors.Is(err, other) {
		t.Fatalf("expected original error passthrough, got %v", err)
	}
}

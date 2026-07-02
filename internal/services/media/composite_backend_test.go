package media

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type sipStub struct {
	startCalls int
	stopCalls  int
	devAddr    string
	startErr   error
	stopErr    error
}

func (s *sipStub) StreamStart(_ context.Context, devAddr string) error {
	s.startCalls++
	s.devAddr = devAddr
	return s.startErr
}

func (s *sipStub) StreamStop(_ context.Context) error {
	s.stopCalls++
	return s.stopErr
}

type commandStub struct {
	startCalls int
	audioPort  int
	videoPort  int
	startErr   error
}

func (c *commandStub) StreamStart(_ context.Context, audioPort, videoPort int) error {
	c.startCalls++
	c.audioPort = audioPort
	c.videoPort = videoPort
	return c.startErr
}

func fixedCallState(state string) func() string {
	return func() string { return state }
}

// --- Legacy path (AV disabled): behaviour identical to before this change ---

func TestCompositeBackendStreamStartUsesSIPWhenAvailable(t *testing.T) {
	sip := &sipStub{}
	cmd := &commandStub{}
	backend := NewCompositeBackend(CompositeBackendOptions{
		SIP: sip, Commands: cmd, AudioPort: 5000, VideoPort: 5007,
	})

	if err := backend.StreamStart(context.Background(), "20"); err != nil {
		t.Fatalf("stream start failed: %v", err)
	}
	if sip.startCalls != 1 || sip.devAddr != "20" {
		t.Fatalf("unexpected sip calls=%d devaddr=%q", sip.startCalls, sip.devAddr)
	}
	if cmd.startCalls != 0 {
		t.Fatalf("expected no command stream start when SIP is available, got %d", cmd.startCalls)
	}
}

func TestCompositeBackendStreamStartUsesCommandsWhenSIPMissing(t *testing.T) {
	cmd := &commandStub{startErr: errors.New("command failed")}
	backend := NewCompositeBackend(CompositeBackendOptions{
		Commands: cmd, AudioPort: 5000, VideoPort: 5007,
	})

	err := backend.StreamStart(context.Background(), "20")
	if err == nil {
		t.Fatal("expected stream start error")
	}
	if cmd.startCalls != 1 {
		t.Fatalf("expected command stream start attempt, got %d", cmd.startCalls)
	}
}

func TestCompositeBackendStreamStopCallsSIP(t *testing.T) {
	sip := &sipStub{}
	backend := NewCompositeBackend(CompositeBackendOptions{
		SIP: sip, AudioPort: 5000, VideoPort: 5007,
	})

	if err := backend.StreamStop(context.Background()); err != nil {
		t.Fatalf("stream stop failed: %v", err)
	}
	if sip.stopCalls != 1 {
		t.Fatalf("expected sip stop call, got %d", sip.stopCalls)
	}
}

// --- AV endpoint path (C100X): call-state gate + 486 fallback ---

func TestCompositeBackendIdleRunsSIPThenAV(t *testing.T) {
	sip := &sipStub{}
	av := &commandStub{}
	backend := NewCompositeBackend(CompositeBackendOptions{
		SIP: sip, AV: av, CallState: fixedCallState("idle"),
		AudioPort: 5000, VideoPort: 5007,
	})

	if err := backend.StreamStart(context.Background(), "12"); err != nil {
		t.Fatalf("stream start failed: %v", err)
	}
	if sip.startCalls != 1 || sip.devAddr != "12" {
		t.Fatalf("expected one sip start with devaddr 12, got calls=%d devaddr=%q", sip.startCalls, sip.devAddr)
	}
	if av.startCalls != 1 || av.audioPort != 5000 || av.videoPort != 5007 {
		t.Fatalf("expected av start with ingest ports, got calls=%d a=%d v=%d", av.startCalls, av.audioPort, av.videoPort)
	}
	if err := backend.StreamStop(context.Background()); err != nil {
		t.Fatalf("stream stop failed: %v", err)
	}
	if sip.stopCalls != 1 {
		t.Fatalf("expected BYE after idle-start, got %d stop calls", sip.stopCalls)
	}
}

func TestCompositeBackendRingingSkipsSIP(t *testing.T) {
	sip := &sipStub{}
	av := &commandStub{}
	backend := NewCompositeBackend(CompositeBackendOptions{
		SIP: sip, AV: av, CallState: fixedCallState("ringing"),
		AudioPort: 5000, VideoPort: 5007,
	})

	if err := backend.StreamStart(context.Background(), "12"); err != nil {
		t.Fatalf("stream start failed: %v", err)
	}
	if sip.startCalls != 0 {
		t.Fatalf("SIP must never be invited during a ring, got %d calls", sip.startCalls)
	}
	if av.startCalls != 1 {
		t.Fatalf("expected av-only start, got %d", av.startCalls)
	}
	if err := backend.StreamStop(context.Background()); err != nil {
		t.Fatalf("stream stop failed: %v", err)
	}
	if sip.stopCalls != 0 {
		t.Fatalf("no BYE expected when we never opened the call, got %d", sip.stopCalls)
	}
}

func TestCompositeBackendActiveCallSkipsSIP(t *testing.T) {
	sip := &sipStub{}
	av := &commandStub{}
	backend := NewCompositeBackend(CompositeBackendOptions{
		SIP: sip, AV: av, CallState: fixedCallState("active"),
		AudioPort: 5000, VideoPort: 5007,
	})

	if err := backend.StreamStart(context.Background(), "12"); err != nil {
		t.Fatalf("stream start failed: %v", err)
	}
	if sip.startCalls != 0 || av.startCalls != 1 {
		t.Fatalf("expected av-only start, sip=%d av=%d", sip.startCalls, av.startCalls)
	}
}

func TestCompositeBackend486FallsThroughToAV(t *testing.T) {
	sip := &sipStub{startErr: fmt.Errorf("%w: busy", ErrSIPCallInProgress)}
	av := &commandStub{}
	backend := NewCompositeBackend(CompositeBackendOptions{
		SIP: sip, AV: av, CallState: fixedCallState("idle"), // stale state: gate said idle, intercom is busy
		AudioPort: 5000, VideoPort: 5007,
	})

	if err := backend.StreamStart(context.Background(), "12"); err != nil {
		t.Fatalf("486 must not fail the stream start: %v", err)
	}
	if av.startCalls != 1 {
		t.Fatalf("expected av start after 486, got %d", av.startCalls)
	}
	if err := backend.StreamStop(context.Background()); err != nil {
		t.Fatalf("stream stop failed: %v", err)
	}
	if sip.stopCalls != 0 {
		t.Fatalf("no BYE expected after 486 (call not ours), got %d", sip.stopCalls)
	}
}

func TestCompositeBackendOtherSIPErrorStillTriesAV(t *testing.T) {
	sip := &sipStub{startErr: errors.New("flexisip unreachable")}
	av := &commandStub{}
	backend := NewCompositeBackend(CompositeBackendOptions{
		SIP: sip, AV: av, CallState: fixedCallState("idle"),
		AudioPort: 5000, VideoPort: 5007,
	})

	if err := backend.StreamStart(context.Background(), "12"); err != nil {
		t.Fatalf("AV success must mask the SIP best-effort failure: %v", err)
	}
	if av.startCalls != 1 {
		t.Fatalf("expected av attempt, got %d", av.startCalls)
	}
}

func TestCompositeBackendBothLegsFailingReturnsError(t *testing.T) {
	sip := &sipStub{startErr: errors.New("flexisip unreachable")}
	av := &commandStub{startErr: errors.New("30007 refused")}
	backend := NewCompositeBackend(CompositeBackendOptions{
		SIP: sip, AV: av, CallState: fixedCallState("idle"),
		AudioPort: 5000, VideoPort: 5007,
	})

	err := backend.StreamStart(context.Background(), "12")
	if err == nil {
		t.Fatal("expected error when both SIP and AV fail")
	}
}

func TestCompositeBackendAVFailureAfterSIPSuccessCleansUp(t *testing.T) {
	sip := &sipStub{}
	av := &commandStub{startErr: errors.New("30007 refused")}
	backend := NewCompositeBackend(CompositeBackendOptions{
		SIP: sip, AV: av, CallState: fixedCallState("idle"),
		AudioPort: 5000, VideoPort: 5007,
	})

	err := backend.StreamStart(context.Background(), "12")
	if err == nil {
		t.Fatal("expected error when AV fails")
	}
	if sip.stopCalls != 1 {
		t.Fatalf("expected SIP cleanup BYE after AV failure, got %d", sip.stopCalls)
	}
}

func TestCompositeBackendNilCallStateMeansIdle(t *testing.T) {
	sip := &sipStub{}
	av := &commandStub{}
	backend := NewCompositeBackend(CompositeBackendOptions{
		SIP: sip, AV: av, AudioPort: 5000, VideoPort: 5007,
	})

	if err := backend.StreamStart(context.Background(), "12"); err != nil {
		t.Fatalf("stream start failed: %v", err)
	}
	if sip.startCalls != 1 || av.startCalls != 1 {
		t.Fatalf("expected idle behaviour with nil CallState, sip=%d av=%d", sip.startCalls, av.startCalls)
	}
}

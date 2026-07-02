package sipadapter

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/domain/event"
	"bticino-go-companion/internal/services/media"
)

func TestManagerDisabledLifecycle(t *testing.T) {
	cfg := config.Default()
	cfg.MediaSIPEnabled = false

	m := NewManager(cfg, log.New(io.Discard, "", 0))
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("disabled start should be no-op: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("disabled close should be no-op: %v", err)
	}
}

func TestManagerDisabledHangupReturnsNoActiveCall(t *testing.T) {
	cfg := config.Default()
	cfg.MediaSIPEnabled = false
	m := NewManager(cfg, log.New(io.Discard, "", 0))

	if err := m.Hangup(context.Background()); err != ErrNoActiveCall {
		t.Fatalf("expected ErrNoActiveCall, got %v", err)
	}
}

func TestManagerDisabledAnswerReturnsNoIncomingCall(t *testing.T) {
	cfg := config.Default()
	cfg.MediaSIPEnabled = false
	m := NewManager(cfg, log.New(io.Discard, "", 0))

	if err := m.Answer(context.Background()); err != ErrNoIncomingCall {
		t.Fatalf("expected ErrNoIncomingCall, got %v", err)
	}
}

func TestInviteAuthUserFallbacksToFromUser(t *testing.T) {
	cfg := config.Default()
	cfg.MediaSIPAuthUser = ""
	cfg.MediaSIPFrom = "c300x@127.0.0.1"
	if got := inviteAuthUser(cfg); got != "c300x" {
		t.Fatalf("unexpected invite auth user: %s", got)
	}

	cfg.MediaSIPAuthUser = "explicit-user"
	if got := inviteAuthUser(cfg); got != "explicit-user" {
		t.Fatalf("unexpected explicit invite auth user: %s", got)
	}
}

func TestNormalizeTransport(t *testing.T) {
	tests := map[string]string{
		"udp":     "udp",
		" TCP ":   "tcp",
		"wss":     "wss",
		"invalid": "tcp",
		"":        "tcp",
	}
	for in, want := range tests {
		if got := normalizeTransport(in); got != want {
			t.Fatalf("normalizeTransport(%q)=%q want=%q", in, got, want)
		}
	}
}

func TestHostFromListen(t *testing.T) {
	host, port := hostFromListen(":5070")
	if host != "" || port != 5070 {
		t.Fatalf("unexpected host/port for :5070 -> host=%q port=%d", host, port)
	}

	host, port = hostFromListen("0.0.0.0:5080")
	if host != "0.0.0.0" || port != 5080 {
		t.Fatalf("unexpected host/port for full address -> host=%q port=%d", host, port)
	}

	host, port = hostFromListen("invalid")
	if host != "" || port != 0 {
		t.Fatalf("expected zero host/port for invalid input, got host=%q port=%d", host, port)
	}
}

func TestBuildContactHeaderAndFormatContactValue(t *testing.T) {
	cfg := config.Default()
	cfg.MediaSIPFrom = ""
	cfg.MediaSIPListen = ":5071"

	header := buildContactHeader(cfg)
	if header.Address.User != "webrtc" {
		t.Fatalf("expected default user webrtc, got %q", header.Address.User)
	}
	if header.Address.Host != "127.0.0.1" {
		t.Fatalf("expected fallback host 127.0.0.1, got %q", header.Address.Host)
	}
	if header.Address.Port != 5071 {
		t.Fatalf("expected port 5071 from listen address, got %d", header.Address.Port)
	}

	val := formatContactValue(header)
	if val != "<sip:webrtc@127.0.0.1:5071>" {
		t.Fatalf("unexpected formatted contact value: %s", val)
	}
}

func TestRegisterDomain(t *testing.T) {
	cfg := config.Default()
	cfg.MediaSIPDomain = "example.local"
	if got := registerDomain(cfg); got != "example.local" {
		t.Fatalf("expected explicit domain, got %q", got)
	}

	cfg.MediaSIPDomain = ""
	cfg.MediaSIPTo = "invalid-sip-target"
	if got := registerDomain(cfg); got != "" {
		t.Fatalf("expected empty domain when target is invalid, got %q", got)
	}
}

func TestShouldRegister(t *testing.T) {
	cfg := config.Default()
	cfg.MediaSIPDomain = "example.local"
	m := &Manager{
		cfg:           cfg,
		client:        &sipgo.Client{},
		registerEvery: 2 * time.Second,
	}

	if !m.shouldRegister(true) {
		t.Fatal("expected force=true to allow register")
	}

	m.lastRegister = time.Now()
	if m.shouldRegister(false) {
		t.Fatal("expected no register immediately after last register")
	}

	m.lastRegister = time.Now().Add(-3 * time.Second)
	if !m.shouldRegister(false) {
		t.Fatal("expected register after interval elapsed")
	}

	m.incoming = &sipgo.DialogServerSession{}
	if m.shouldRegister(true) {
		t.Fatal("expected busy state to block register")
	}
}

func TestIncomingInviteExpires(t *testing.T) {
	events := make(chan event.Envelope, 1)
	dlg := &sipgo.DialogServerSession{}
	m := &Manager{
		enabled:         true,
		incoming:        dlg,
		incomingTimeout: 5 * time.Millisecond,
		sink: func(ev event.Envelope) {
			events <- ev
		},
	}

	m.mu.Lock()
	m.startIncomingExpiryLocked(dlg)
	m.mu.Unlock()

	select {
	case ev := <-events:
		if ev.Type != event.TypeCallEnded {
			t.Fatalf("unexpected event type: %s", ev.Type)
		}
		if got := ev.Payload["reason"]; got != "incoming_timeout" {
			t.Fatalf("unexpected reason: %v", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected incoming timeout event")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.incoming != nil {
		t.Fatal("expected incoming dialog to be cleared")
	}
	if m.incomingExpiry != nil {
		t.Fatal("expected incoming expiry timer to be cleared")
	}
}

func TestIncomingInviteExpiryCanBeStopped(t *testing.T) {
	dlg := &sipgo.DialogServerSession{}
	m := &Manager{
		enabled:         true,
		incoming:        dlg,
		incomingTimeout: 5 * time.Millisecond,
		sink: func(ev event.Envelope) {
			t.Fatalf("unexpected event after stopped expiry: %+v", ev)
		},
	}

	m.mu.Lock()
	m.startIncomingExpiryLocked(dlg)
	m.stopIncomingExpiryLocked()
	m.mu.Unlock()

	time.Sleep(25 * time.Millisecond)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.incoming != dlg {
		t.Fatal("expected incoming dialog to remain after stopped expiry")
	}
}

func TestClassifyInviteAnswerError(t *testing.T) {
	busyPtr := &sipgo.ErrDialogResponse{Res: &sip.Response{StatusCode: 486}}
	if err := classifyInviteAnswerError(busyPtr); !errors.Is(err, media.ErrSIPCallInProgress) {
		t.Fatalf("expected ErrSIPCallInProgress for pointer-form 486 (how sipgo returns it), got: %v", err)
	}

	busyVal := sipgo.ErrDialogResponse{Res: &sip.Response{StatusCode: 486}}
	if err := classifyInviteAnswerError(busyVal); !errors.Is(err, media.ErrSIPCallInProgress) {
		t.Fatalf("expected ErrSIPCallInProgress for value-form 486, got: %v", err)
	}

	notFound := &sipgo.ErrDialogResponse{Res: &sip.Response{StatusCode: 404}}
	if err := classifyInviteAnswerError(notFound); errors.Is(err, media.ErrSIPCallInProgress) {
		t.Fatalf("404 must not map to ErrSIPCallInProgress: %v", err)
	}

	if err := classifyInviteAnswerError(errors.New("plain transport failure")); errors.Is(err, media.ErrSIPCallInProgress) {
		t.Fatalf("generic error must not map to ErrSIPCallInProgress: %v", err)
	}

	nilRes := &sipgo.ErrDialogResponse{}
	if err := classifyInviteAnswerError(nilRes); errors.Is(err, media.ErrSIPCallInProgress) {
		t.Fatalf("nil Res must not map to ErrSIPCallInProgress: %v", err)
	}
}

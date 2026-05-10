package sipadapter

import (
	"context"
	"io"
	"log"
	"testing"

	"bticino-go-companion/internal/config"
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

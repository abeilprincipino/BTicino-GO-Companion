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

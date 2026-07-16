package signaling

import (
	"errors"
	"testing"
)

func TestResolveStreamProfile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		model      string
		wantTarget string
		wantErr    error
	}{
		{name: "c300x", model: " C300X ", wantTarget: "c300x@127.0.0.1"},
		{name: "c100x", model: "c100x", wantTarget: "c100x@127.0.0.1"},
		{name: "unsupported", model: "C200X", wantErr: ErrUnsupportedStreamModel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile, err := ResolveStreamProfile(tt.model)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ResolveStreamProfile(%q) error = %v, want %v", tt.model, err, tt.wantErr)
			}
			if profile.DefaultTarget != tt.wantTarget {
				t.Fatalf("ResolveStreamProfile(%q) target = %q, want %q", tt.model, profile.DefaultTarget, tt.wantTarget)
			}
		})
	}
}

func TestResolveInviteTargetUsesProfileEndpointAndDomain(t *testing.T) {
	t.Parallel()

	target, err := resolveInviteTarget("c300x@127.0.0.1", "example.local")
	if err != nil {
		t.Fatalf("resolveInviteTarget() error = %v", err)
	}
	if target.URI.User != "c300x" || target.URI.Host != "example.local" {
		t.Fatalf("target URI = %s, want sip:c300x@example.local", target.URI.String())
	}
	if target.destination != "127.0.0.1:5060" {
		t.Fatalf("target destination = %q, want 127.0.0.1:5060", target.destination)
	}
}

func TestNewStreamDialerRejectsUnsupportedModel(t *testing.T) {
	t.Parallel()

	_, err := NewStreamDialer(StreamDialerConfig{Model: "unsupported"})
	if !errors.Is(err, ErrUnsupportedStreamModel) {
		t.Fatalf("NewStreamDialer() error = %v, want %v", err, ErrUnsupportedStreamModel)
	}
}

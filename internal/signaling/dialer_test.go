package signaling

import "testing"

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

func TestNewStreamDialerRequiresTarget(t *testing.T) {
	t.Parallel()

	_, err := NewStreamDialer(StreamDialerConfig{})
	if err != ErrStreamTargetUnset {
		t.Fatalf("NewStreamDialer() error = %v, want %v", err, ErrStreamTargetUnset)
	}
}

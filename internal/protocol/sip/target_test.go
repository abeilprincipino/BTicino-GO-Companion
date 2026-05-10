package sip

import "testing"

func TestResolveInviteTarget(t *testing.T) {
	target, err := ResolveInviteTarget("c300x@127.0.0.1", "example.local", true)
	if err != nil {
		t.Fatalf("resolve target failed: %v", err)
	}
	if target.URI.User != "c300x" {
		t.Fatalf("unexpected user: %s", target.URI.User)
	}
	if target.URI.Host != "example.local" {
		t.Fatalf("unexpected host rewrite: %s", target.URI.Host)
	}
	if target.Destination != "127.0.0.1:5060" {
		t.Fatalf("unexpected destination: %s", target.Destination)
	}
	if !target.AddDevAddr {
		t.Fatal("expected AddDevAddr true")
	}
}

func TestParseFromAddress(t *testing.T) {
	user, host, port := ParseFromAddress("sip:webrtc@127.0.0.1:5070")
	if user != "webrtc" || host != "127.0.0.1" || port != 5070 {
		t.Fatalf("unexpected parse: %s %s %d", user, host, port)
	}
}

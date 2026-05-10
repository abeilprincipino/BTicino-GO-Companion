package openwebnet

import "testing"

func TestFrameBuilders(t *testing.T) {
	if got := BuildUnlockOpen("21"); got != "*8*19*21##" {
		t.Fatalf("unexpected open frame: %s", got)
	}
	if got := BuildUnlockClose("21"); got != "*8*20*21##" {
		t.Fatalf("unexpected close frame: %s", got)
	}
}

func TestFramePredicates(t *testing.T) {
	if !IsRingStart("*8*1#1#4#10*21##") {
		t.Fatal("expected ring start")
	}
	if !IsFloorRingStart("*7*59#12#0#0*##") {
		t.Fatal("expected floor ring start")
	}
	if !IsStreamStop("*7*0*##") {
		t.Fatal("expected stream stop")
	}
	if !IsStreamStartVideo("*7*300#127#0#0#1#5007#0*##") {
		t.Fatal("expected video start")
	}
	if !IsStreamStartAudio("*7*300#127#0#0#1#5000#2*##") {
		t.Fatal("expected audio start")
	}
}

package openwebnetproto

import "testing"

func TestFrameBuilders(t *testing.T) {
	if got := BuildUnlockOpen("21"); got != "*8*19*21##" {
		t.Fatalf("unexpected open frame: %s", got)
	}
	if got := BuildUnlockClose("21"); got != "*8*20*21##" {
		t.Fatalf("unexpected close frame: %s", got)
	}
	if got := BuildStreamStartVideo(5007); got != "*7*300#127#0#0#1#5007#0*##" {
		t.Fatalf("unexpected stream video frame: %s", got)
	}
	if got := BuildStreamStartAudio(5000); got != "*7*300#127#0#0#1#5000#2*##" {
		t.Fatalf("unexpected stream audio frame: %s", got)
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

func TestExtractAddress(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "ring_start", raw: "*8*1#1#4#10*21##", want: "21"},
		{name: "view_request_target", raw: "*8*1#5#4#21*12##", want: "21"},
		{name: "unlock_open", raw: "*8*19*22##", want: "22"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractAddress(tc.raw); got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

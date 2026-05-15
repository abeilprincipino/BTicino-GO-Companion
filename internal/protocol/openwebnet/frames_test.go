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

func TestParseVoicemailStatus(t *testing.T) {
	enabled, welcomeEnabled, ok := ParseVoicemailStatus("*#8**40*1*0*0153*1*25##")
	if !ok {
		t.Fatal("expected voicemail status frame to parse")
	}
	if !enabled {
		t.Fatal("expected voicemail enabled")
	}
	if welcomeEnabled {
		t.Fatal("expected welcome message disabled")
	}

	enabled, welcomeEnabled, ok = ParseVoicemailStatus("*#8**40*0*1*0153*1*25##")
	if !ok {
		t.Fatal("expected voicemail status frame to parse")
	}
	if enabled {
		t.Fatal("expected voicemail disabled")
	}
	if !welcomeEnabled {
		t.Fatal("expected welcome message enabled")
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

func TestParseDiagnosticFrames(t *testing.T) {
	ip, ok := ParseDiagnosticIP("*#13**10*192*0*2*172##")
	if !ok || ip != "192.0.2.172" {
		t.Fatalf("unexpected ip parse: ok=%v ip=%q", ok, ip)
	}

	netmask, ok := ParseDiagnosticNetmask("*#13**11*255*255*255*0##")
	if !ok || netmask != "255.255.255.0" {
		t.Fatalf("unexpected netmask parse: ok=%v netmask=%q", ok, netmask)
	}

	mac, ok := ParseDiagnosticMAC("*#13**12*0*17*34*51*68*85##")
	if !ok || mac != "00:11:22:33:44:55" {
		t.Fatalf("unexpected mac parse: ok=%v mac=%q", ok, mac)
	}

	firmware, ok := ParseDiagnosticFirmware("*#13**16*9*8*7##")
	if !ok || firmware != "9.8.7" {
		t.Fatalf("unexpected firmware parse: ok=%v fw=%q", ok, firmware)
	}

	hardware, ok := ParseDiagnosticHardware("*#13**17*3*2*1##")
	if !ok || hardware != "3.2.1" {
		t.Fatalf("unexpected hardware parse: ok=%v hw=%q", ok, hardware)
	}

	kernel, ok := ParseDiagnosticKernel("*#13**23*6*1*2##")
	if !ok || kernel != "6.1.2" {
		t.Fatalf("unexpected kernel parse: ok=%v kernel=%q", ok, kernel)
	}

	distribution, ok := ParseDiagnosticDistribution("*#13**24*1*2*3##")
	if !ok || distribution != "1.2.3" {
		t.Fatalf("unexpected distribution parse: ok=%v distribution=%q", ok, distribution)
	}
}

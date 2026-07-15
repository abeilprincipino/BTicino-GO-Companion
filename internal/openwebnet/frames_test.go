package openwebnet

import "testing"

func TestFrameBuilders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "unlock open trims address", got: BuildUnlockOpen(" 21 "), want: "*8*19*21##"},
		{name: "unlock close", got: BuildUnlockClose("21"), want: "*8*20*21##"},
		{name: "high resolution video", got: BuildStreamStartVideo(5007), want: "*7*300#127#0#0#1#5007#0*##"},
		{name: "low resolution video", got: BuildAVAddStreamVideo("192.168.1.5", 10002, false), want: "*7*300#192#168#1#5#10002#1*##"},
		{name: "audio", got: BuildStreamStartAudio(5000), want: "*7*300#127#0#0#1#5000#2*##"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.got != test.want {
				t.Fatalf("got %q, want %q", test.got, test.want)
			}
		})
	}
}

func TestFramePredicatesAndExtractors(t *testing.T) {
	t.Parallel()

	if !IsRingStart(" *8*1#1#4#10*21## ") || !IsViewRequest("*8*1#5#4#21*12##") {
		t.Fatal("expected ring and view frames to be recognized")
	}

	if !IsStreamStartVideo("*7*300#127#0#0#1#5007#1*##") || !IsStreamStartAudio("*7*300#127#0#0#1#5000#2*##") {
		t.Fatal("expected stream frames to be recognized")
	}

	if IsStreamStartAudio("*7*300#127#0#0#1#5007#0*##") {
		t.Fatal("video stream must not be classified as audio")
	}

	if where, ok := ParseReceiveVideoWhere("*7*0*4001##"); !ok || where != "4001" {
		t.Fatalf("unexpected receive-video parse: where=%q ok=%t", where, ok)
	}

	if IsReceiveVideo(FrameStop) {
		t.Fatal("stream stop must not be classified as receive-video")
	}

	if address := ExtractAddress("*8*1#5#4#21*12##"); address != "21" {
		t.Fatalf("got address %q, want %q", address, "21")
	}

	if address, ok := ParseRingIdentityAddress("*8*9#1#4*22#2##"); !ok || address != "22" {
		t.Fatalf("unexpected ring identity parse: address=%q ok=%t", address, ok)
	}
}

func TestParseDiagnosticFrames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		parse func(string) (string, bool)
		frame string
		want  string
	}{
		{name: "ip", parse: ParseDiagnosticIP, frame: "*#13**10*192*0*2*172##", want: "192.0.2.172"},
		{name: "netmask", parse: ParseDiagnosticNetmask, frame: "*#13**11*255*255*255*0##", want: "255.255.255.0"},
		{name: "firmware", parse: ParseDiagnosticFirmware, frame: "*#13**16*9*8*7##", want: "9.8.7"},
		{name: "mac", parse: ParseDiagnosticMAC, frame: "*#13**12*0*17*34*51*68*85##", want: "00:11:22:33:44:55"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, ok := test.parse(test.frame)
			if !ok || got != test.want {
				t.Fatalf("got %q, %t; want %q, true", got, ok, test.want)
			}
		})
	}

	if _, ok := ParseDiagnosticMAC("*#13**12*0*17*34*51*68*256##"); ok {
		t.Fatal("out-of-range MAC octet must be rejected")
	}
}

func TestParseVoicemailStatus(t *testing.T) {
	t.Parallel()

	enabled, welcomeEnabled, ok := ParseVoicemailStatus("*#8**40*1*0*0153*1*25##")
	if !ok || !enabled || welcomeEnabled {
		t.Fatalf("unexpected voicemail status: enabled=%t welcome=%t ok=%t", enabled, welcomeEnabled, ok)
	}

	if _, _, ok := ParseVoicemailStatus("*#8**40*2*0##"); ok {
		t.Fatal("invalid voicemail status must be rejected")
	}
}

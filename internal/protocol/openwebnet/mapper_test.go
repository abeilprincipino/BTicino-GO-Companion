package openwebnetproto

import "testing"

func TestMapperCoreFrames(t *testing.T) {
	mapper := NewMapper()
	cases := []struct {
		name      string
		raw       string
		wantTypes []string
	}{
		{name: "incoming_call", raw: "*8*1#1#4#10*21##", wantTypes: []string{"ring.started", "call.incoming"}},
		{name: "view_request", raw: "*8*1#5#4#10*21##", wantTypes: []string{"call.view_requested"}},
		{name: "unlock_open", raw: "*8*19*21##", wantTypes: []string{"unlock.pulse.started"}},
		{name: "unlock_close", raw: "*8*20*21##", wantTypes: []string{"unlock.pulse.ended"}},
		{name: "mute", raw: "*#8**33*0##", wantTypes: []string{"audio.muted"}},
		{name: "unmute", raw: "*#8**33*1##", wantTypes: []string{"audio.unmuted"}},
		{name: "voicemail_enabled", raw: "*#8**40*1*0*0153*1*25##", wantTypes: []string{"voicemail.enabled"}},
		{name: "voicemail_disabled", raw: "*#8**40*0*1*0153*1*25##", wantTypes: []string{"voicemail.disabled"}},
		{name: "video_stream_start", raw: "*7*300#127#0#0#1#5007#0*##", wantTypes: []string{"stream.started"}},
		{name: "audio_stream_start", raw: "*7*300#127#0#0#1#5000#2*##", wantTypes: []string{"stream.started"}},
		{name: "receive_video", raw: "*7*0*4001##", wantTypes: []string{"call.view_requested"}},
		{name: "stream_probe_ignored", raw: "*7*73#0#0*##", wantTypes: nil},
		{name: "stream_stop", raw: "*7*0*##", wantTypes: []string{"stream.stopped", "ring.ended", "call.ended"}},
		{name: "free_av_resources_stop", raw: "*7*9**##", wantTypes: []string{"stream.stopped", "ring.ended", "call.ended"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := mapper.Map(Message{System: "OPEN", Raw: tc.raw})
			if len(events) != len(tc.wantTypes) {
				t.Fatalf("expected %d events, got %d", len(tc.wantTypes), len(events))
			}
			for i, wantType := range tc.wantTypes {
				if events[i].Type != wantType {
					t.Fatalf("event %d expected %s, got %s", i, wantType, events[i].Type)
				}
			}
		})
	}
}

func TestMapperFloorRingLifecycle(t *testing.T) {
	mapper := NewMapper()
	events := mapper.Map(Message{System: "OPEN", Raw: "*7*59#12#0#0*##"})
	if len(events) != 1 || events[0].Type != "ring.floor.started" {
		t.Fatalf("unexpected floor start: %+v", events)
	}
	events = mapper.Map(Message{System: "OPEN", Raw: "*7*0*##"})
	if len(events) != 4 || events[3].Type != "ring.floor.ended" {
		t.Fatalf("unexpected floor end flow: %+v", events)
	}
}

func TestMapperViewRequestUsesRequestedEntrypointAddress(t *testing.T) {
	mapper := NewMapper()
	events := mapper.Map(Message{System: "OPEN", Raw: "*8*1#5#4#21*12##"})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	payload := events[0].Payload
	if payload["devaddr"] != "21" {
		t.Fatalf("expected devaddr 21, got %#v", payload["devaddr"])
	}
}

func TestMapperReceiveVideoCarriesWhere(t *testing.T) {
	mapper := NewMapper()
	events := mapper.Map(Message{System: "OPEN", Raw: "*7*0*4002##"})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "call.view_requested" {
		t.Fatalf("expected call.view_requested, got %s", events[0].Type)
	}
	if events[0].Payload["where"] != "4002" {
		t.Fatalf("expected where 4002, got %#v", events[0].Payload["where"])
	}
}

func TestMapperAcceptsASWMSystem(t *testing.T) {
	mapper := NewMapper()
	events := mapper.Map(Message{System: "aswm", Raw: "*7*0*4001##"})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "call.view_requested" {
		t.Fatalf("expected call.view_requested, got %s", events[0].Type)
	}
}

func TestMapperDeduplicatesImmediateDuplicateFrames(t *testing.T) {
	mapper := NewMapper()
	first := mapper.Map(Message{System: "OPEN", Raw: "*7*9**##"})
	if len(first) == 0 {
		t.Fatal("expected first frame to map events")
	}
	second := mapper.Map(Message{System: "aswm", Raw: "*7*9**##"})
	if len(second) != 0 {
		t.Fatalf("expected duplicate frame to be dropped, got %d events", len(second))
	}
}

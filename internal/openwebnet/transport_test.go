package openwebnet

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"testing"
)

func TestMapperMapsRingAndStopForConfiguredEntrypoint(t *testing.T) {
	mapper := NewMapper([]config.Entrypoint{{ID: "main", DevAddr: "21"}})

	started := mapper.Map(Message{System: "OPEN", Raw: "*8*1#1#4#10*21##"})
	if len(started) != 2 {
		t.Fatalf("started events = %d, want 2", len(started))
	}
	if _, ok := started[0].(core.RingStarted); !ok {
		t.Fatalf("first event = %T, want core.RingStarted", started[0])
	}
	incoming, ok := started[1].(core.IncomingCallStarted)
	if !ok {
		t.Fatalf("second event = %T, want core.IncomingCallStarted", started[1])
	}
	if incoming.EntrypointID != "main" || incoming.DialogID == "" {
		t.Fatalf("incoming event = %#v", incoming)
	}

	if repeated := mapper.Map(Message{System: "OPEN", Raw: "*8*1#1#4#10*21##"}); len(repeated) != 0 {
		t.Fatalf("repeated ring produced %d events", len(repeated))
	}

	stopped := mapper.Map(Message{System: "ASWM", Raw: FrameFreeAVResources})
	if len(stopped) != 2 {
		t.Fatalf("stopped events = %d, want 2", len(stopped))
	}
	if _, ok := stopped[0].(core.RingCleared); !ok {
		t.Fatalf("first stop event = %T, want core.RingCleared", stopped[0])
	}
	ended, ok := stopped[1].(core.CallHungUp)
	if !ok || ended.DialogID != incoming.DialogID {
		t.Fatalf("hangup event = %#v, want dialog %q", stopped[1], incoming.DialogID)
	}
}

func TestMapperIgnoresUnknownEntrypoint(t *testing.T) {
	mapper := NewMapper([]config.Entrypoint{{ID: "main", DevAddr: "21"}, {ID: "side", DevAddr: "22"}})
	if events := mapper.Map(Message{System: "OPEN", Raw: "*8*1#1#4#10*99##"}); len(events) != 0 {
		t.Fatalf("unknown entrypoint produced %d events", len(events))
	}
}

func TestMapperMapsAudioStateFrames(t *testing.T) {
	mapper := NewMapper(nil)

	muted := mapper.Map(Message{System: "OPEN", Raw: FrameAudioMuted})
	if len(muted) != 1 {
		t.Fatalf("muted events = %d, want 1", len(muted))
	}
	if _, ok := muted[0].(core.AudioMuted); !ok {
		t.Fatalf("muted event = %T, want core.AudioMuted", muted[0])
	}

	unmuted := mapper.Map(Message{System: "OPEN", Raw: FrameAudioUnmuted})
	if len(unmuted) != 1 {
		t.Fatalf("unmuted events = %d, want 1", len(unmuted))
	}
	if _, ok := unmuted[0].(core.AudioUnmuted); !ok {
		t.Fatalf("unmuted event = %T, want core.AudioUnmuted", unmuted[0])
	}
}

func TestTraceKeepsMostRecentFrames(t *testing.T) {
	trace := NewTrace(2)
	trace.Record(Message{System: "OPEN", Raw: "first"}, 0)
	trace.Record(Message{System: "OPEN", Raw: "second"}, 1)
	trace.Record(Message{System: "ASWM", Raw: "third"}, 2)

	frames := trace.Frames()
	if len(frames) != 2 || frames[0]["raw"] != "second" || frames[1]["raw"] != "third" {
		t.Fatalf("frames = %#v", frames)
	}
	if mapped, ok := frames[0]["mapped"].(bool); !ok || !mapped {
		t.Fatalf("mapped = %#v", frames[0]["mapped"])
	}
}

package openwebnet

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"testing"
)

func TestMapperEmitsOnlyPhysicalRingEvents(t *testing.T) {
	t.Parallel()

	mapper := NewMapper([]config.Entrypoint{{ID: "main", DevAddr: "21"}})

	events := mapper.Map(Message{System: "open", Raw: "*8*1#1#4#10*21##"})
	if len(events) != 1 {
		t.Fatalf("ring start events = %#v, want exactly one", events)
	}

	if _, ok := events[0].(core.RingStarted); !ok {
		t.Fatalf("event = %#v, want RingStarted", events[0])
	}

	stopEvents := mapper.Map(Message{System: "open", Raw: FrameStop})
	if len(stopEvents) != 1 {
		t.Fatalf("stop events = %#v, want exactly one", stopEvents)
	}

	if _, ok := stopEvents[0].(core.RingCleared); !ok {
		t.Fatalf("event = %#v, want RingCleared", stopEvents[0])
	}
}

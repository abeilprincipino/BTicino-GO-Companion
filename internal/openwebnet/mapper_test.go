package openwebnet

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"testing"
	"time"
)

// The timings are those of a C100X on 2026-11-03, when a second press of the
// doorbell restarted the ring.
func TestMapperKeepsARestartedRingThroughTheOldStopBurst(t *testing.T) {
	t.Parallel()

	const ringFrame = "*8*1#1#4#21*11##"

	start := time.Date(2026, time.November, 3, 17, 8, 14, 0, time.UTC)
	mapper := NewMapper([]config.Entrypoint{{ID: "main", DevAddr: "21"}})

	at := func(milliseconds int, raw string) []core.Event {
		mapper.now = func() time.Time { return start.Add(time.Duration(milliseconds) * time.Millisecond) }
		return mapper.Map(Message{System: "open", Raw: raw})
	}

	if events := at(-11000, ringFrame); len(events) != 1 {
		t.Fatalf("first ring events = %#v, want exactly one", events)
	}

	if events := at(186, FrameStop); len(events) != 1 {
		t.Fatalf("first ring stop events = %#v, want exactly one", events)
	}

	if events := at(350, ringFrame); len(events) != 1 {
		t.Fatalf("second ring events = %#v, want exactly one", events)
	}

	for _, milliseconds := range []int{354, 487, 504, 865, 868} {
		if events := at(milliseconds, FrameStop); len(events) != 0 {
			t.Fatalf("stale stop at +%d ms events = %#v, want none", milliseconds, events)
		}
	}

	events := at(30340, FrameStop)
	if len(events) != 1 {
		t.Fatalf("second ring stop events = %#v, want exactly one", events)
	}

	if _, ok := events[0].(core.RingCleared); !ok {
		t.Fatalf("event = %#v, want RingCleared", events[0])
	}
}

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

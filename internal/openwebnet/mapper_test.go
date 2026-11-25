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

// The frames are those of a C100X with two doorbells on 2026-11-20: every ring
// frame carries the address 21, and the ring identity frame names the unit
// whose camera rings, 21 or 213.
func TestMapperRetargetsARingToTheUnitItsIdentityFrameNames(t *testing.T) {
	t.Parallel()

	doorbells := []config.Entrypoint{
		{ID: "portal0", DevAddr: "21", Capabilities: config.Capabilities{Stream: true, Unlock: true, Ring: true}},
		{ID: "portal6", DevAddr: "213", Capabilities: config.Capabilities{Stream: true, Unlock: true, Ring: true}},
	}

	type frame struct {
		milliseconds int
		raw          string
	}

	for name, test := range map[string]struct {
		frames []frame
		want   []core.Event
	}{
		"the unit at the ring frame's address": {
			frames: []frame{
				{0, "*8*1#1#4#21*110##"}, {418, "*8*1#1#4#21*110##"},
				{2173, "*8*9#1#4*21##"}, {2177, "*8*9#1#4*21##"},
				{11796, FrameStop},
			},
			want: []core.Event{
				core.RingStarted{EntrypointID: "portal0", FrameAddress: "21"},
				core.RingIdentified{EntrypointID: "portal0", Provisional: "portal0", FrameAddress: "21", Delay: 2173 * time.Millisecond},
				core.RingCleared{EntrypointID: "portal0"},
			},
		},
		"another unit": {
			frames: []frame{
				{0, "*8*1#1#4#21*110##"}, {300, "*8*9#1#4*213##"}, {450, "*8*1#1#4#21*110##"},
				{1193, "*8*9#1#4*213##"}, {1194, "*8*9#1#4*213##"},
				{8726, "*8*3#1#4*4110##"}, {9128, FrameStop},
			},
			want: []core.Event{
				core.RingStarted{EntrypointID: "portal0", FrameAddress: "21"},
				core.RingRetargeted{From: "portal0", To: "portal6"},
				core.RingIdentified{EntrypointID: "portal6", Provisional: "portal0", FrameAddress: "21", Delay: 300 * time.Millisecond},
				core.RingCleared{EntrypointID: "portal6"},
			},
		},
		"an identity frame with no ring": {
			frames: []frame{{0, "*8*9#1#4*213##"}},
		},
		"a unit no doorbell is at": {
			frames: []frame{{0, "*8*1#1#4#21*110##"}, {300, "*8*9#1#4*22##"}, {9000, FrameStop}},
			want: []core.Event{
				core.RingStarted{EntrypointID: "portal0", FrameAddress: "21"},
				core.RingIdentified{Provisional: "portal0", FrameAddress: "21", Delay: 300 * time.Millisecond},
				core.RingCleared{EntrypointID: "portal0"},
			},
		},
		// An installation whose ring frames carry the unit that rang: the
		// identity frame only confirms it.
		"a ring frame that is right": {
			frames: []frame{{0, "*8*1#1#4#213*110##"}, {350, "*8*9#1#4*213##"}, {9000, FrameStop}},
			want: []core.Event{
				core.RingStarted{EntrypointID: "portal6", FrameAddress: "213"},
				core.RingIdentified{EntrypointID: "portal6", Provisional: "portal6", FrameAddress: "213", Delay: 350 * time.Millisecond},
				core.RingCleared{EntrypointID: "portal6"},
			},
		},
		// An intercom that never names the unit.
		"no identity frame": {
			frames: []frame{{0, "*8*1#1#4#21*110##"}, {9000, FrameStop}},
			want:   []core.Event{core.RingStarted{EntrypointID: "portal0", FrameAddress: "21"}, core.RingCleared{EntrypointID: "portal0"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			start := time.Date(2026, time.November, 20, 17, 28, 58, 0, time.UTC)
			mapper := NewMapper(doorbells)

			var got []core.Event

			for _, frame := range test.frames {
				mapper.now = func() time.Time { return start.Add(time.Duration(frame.milliseconds) * time.Millisecond) }
				got = append(got, mapper.Map(Message{System: "open", Raw: frame.raw})...)
			}

			if len(got) != len(test.want) {
				t.Fatalf("events = %#v, want %#v", got, test.want)
			}

			for i := range test.want {
				if got[i] != test.want[i] {
					t.Fatalf("events = %#v, want %#v", got, test.want)
				}
			}
		})
	}
}

func TestMapperKeepsEveryRingOnTheNamedDoorbell(t *testing.T) {
	t.Parallel()

	mapper := NewMapper([]config.Entrypoint{
		{ID: "main", DevAddr: "21", Capabilities: config.Capabilities{Stream: true, Ring: true}},
		{ID: "gate", DevAddr: "213", Capabilities: config.Capabilities{Unlock: true}},
	})

	if events := mapper.Map(Message{System: "open", Raw: "*8*1#1#4#21*110##"}); len(events) != 1 {
		t.Fatalf("ring events = %#v, want exactly one", events)
	}

	if events := mapper.Map(Message{System: "open", Raw: "*8*9#1#4*213##"}); len(events) != 0 {
		t.Fatalf("identity events = %#v, want none: the only doorbell keeps the ring", events)
	}
}

// The frames of a C100X on 2026-11-23 at 18:46:55, when a Sfera panel ended its
// own call and called again with its new ring frame before the stop frame of
// the old ring.
func TestMapperStartsTheRingOfACallThatFollowsTheBusCallEnd(t *testing.T) {
	t.Parallel()

	const ringFrame = "*8*1#1#4#21*110##"

	start := time.Date(2026, time.November, 23, 18, 46, 46, 91_000_000, time.UTC)
	mapper := NewMapper([]config.Entrypoint{{ID: "portal0", DevAddr: "21", Capabilities: config.Capabilities{Stream: true, Ring: true}}})

	at := func(milliseconds int, raw string) []core.Event {
		mapper.now = func() time.Time { return start.Add(time.Duration(milliseconds) * time.Millisecond) }
		return mapper.Map(Message{System: "open", Raw: raw})
	}

	if events := at(0, ringFrame); len(events) != 1 {
		t.Fatalf("first ring events = %#v, want exactly one", events)
	}

	// A repeat of the ring frame while the call is up is the same ring.
	if events := at(401, ringFrame); len(events) != 0 {
		t.Fatalf("repeated ring frame events = %#v, want none", events)
	}

	if events := at(9751, "*8*3#1#3*421##"); len(events) != 0 {
		t.Fatalf("call end events = %#v, want none", events)
	}

	events := at(10000, ringFrame)
	if len(events) != 2 {
		t.Fatalf("new call events = %#v, want the old ring cleared and the new one started", events)
	}

	if _, ok := events[0].(core.RingCleared); !ok {
		t.Fatalf("first event = %#v, want RingCleared", events[0])
	}

	if started, ok := events[1].(core.RingStarted); !ok || started.EntrypointID != "portal0" {
		t.Fatalf("second event = %#v, want RingStarted on portal0", events[1])
	}

	// The old ring's late stop frames do not clear the new ring.
	for _, milliseconds := range []int{35, 531, 532, 576} {
		if events := at(10000+milliseconds, FrameStop); len(events) != 0 {
			t.Fatalf("stale stop at +%d ms events = %#v, want none", milliseconds, events)
		}
	}

	if events := at(19800, FrameStop); len(events) != 1 {
		t.Fatalf("new ring stop events = %#v, want exactly one", events)
	}

	// Once that ring is over, a ring frame is a new ring, as always.
	if events := at(40000, ringFrame); len(events) != 1 {
		t.Fatalf("next ring events = %#v, want exactly one", events)
	}
}

// An indoor unit's hangup of a view (type 5) does not end a ring.
func TestMapperKeepsTheRingThroughTheEndOfAView(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.November, 23, 18, 47, 40, 0, time.UTC)
	mapper := NewMapper([]config.Entrypoint{{ID: "portal0", DevAddr: "21", Capabilities: config.Capabilities{Stream: true, Ring: true}}})

	at := func(milliseconds int, raw string) []core.Event {
		mapper.now = func() time.Time { return start.Add(time.Duration(milliseconds) * time.Millisecond) }
		return mapper.Map(Message{System: "open", Raw: raw})
	}

	at(0, "*8*1#1#4#21*110##")
	at(500, "*8*3#5#4*4110##")

	if events := at(2000, "*8*1#1#4#21*110##"); len(events) != 0 {
		t.Fatalf("ring frame after a view end events = %#v, want none: still the same ring", events)
	}
}

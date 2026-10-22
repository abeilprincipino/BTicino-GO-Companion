package media

import (
	"errors"
	"testing"

	"github.com/pion/rtp"
)

func h264Packet(sequence uint16, timestamp uint32, payload ...byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: sequence, Timestamp: timestamp}, Payload: payload}
}

// Payload starts: NAL header byte, NAL type in the low five bits.
var (
	idrSlice     = []byte{0x65, 1}       // type 5
	spsUnit      = []byte{0x67, 1}       // type 7
	ppsUnit      = []byte{0x68, 1}       // type 8
	plainSlice   = []byte{0x41, 1}       // type 1
	fuAIDRStart  = []byte{0x7c, 0x85, 1} // FU-A, start bit, type 5
	fuAIDRMiddle = []byte{0x7c, 0x05, 1} // FU-A, no start bit, type 5
)

func TestStartsKeyframe(t *testing.T) {
	stapA := []byte{0x78, 0x00, 0x02, 0x67, 0x01, 0x00, 0x02, 0x68, 0x01} // STAP-A: SPS, PPS
	stapANoKey := []byte{0x78, 0x00, 0x02, 0x41, 0x01}                    // STAP-A: one slice
	stapATruncated := []byte{0x78, 0x00, 0x09, 0x67}                      // size past the end

	tests := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"IDR", idrSlice, true},
		{"SPS", spsUnit, true},
		{"PPS", ppsUnit, false},
		{"slice", plainSlice, false},
		{"FU-A IDR start", fuAIDRStart, true},
		{"FU-A IDR middle", fuAIDRMiddle, false},
		{"STAP-A with SPS", stapA, true},
		{"STAP-A without key", stapANoKey, false},
		{"STAP-A truncated", stapATruncated, false},
		{"empty", nil, false},
	}

	for _, test := range tests {
		if got := startsKeyframe(test.payload); got != test.want {
			t.Errorf("%s: startsKeyframe() = %t, want %t", test.name, got, test.want)
		}
	}
}

type recordedVideo struct{ sequences []uint16 }

func (r *recordedVideo) events() SourceEvents {
	return SourceEvents{VideoRTP: func(packet *rtp.Packet) { r.sequences = append(r.sequences, packet.SequenceNumber) }}
}

func assertSequences(t *testing.T, name string, got []uint16, want ...uint16) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%s got sequences %v, want %v", name, got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s got sequences %v, want %v", name, got, want)
		}
	}
}

func TestFanoutDeliversToEveryConsumerInJoinOrder(t *testing.T) {
	f := newFanout()

	var order []string

	f.add(1, SourceEvents{AudioRTP: func(*rtp.Packet) { order = append(order, "first") }})
	f.add(2, SourceEvents{AudioRTP: func(*rtp.Packet) { order = append(order, "second") }})

	f.audio(h264Packet(1, 1))

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("delivery order = %v, want [first second]", order)
	}
}

func TestFanoutReplaysTheKeyframeToAJoinerWithoutGaps(t *testing.T) {
	f := newFanout()
	first, late := &recordedVideo{}, &recordedVideo{}

	f.add(1, first.events())
	f.video(h264Packet(1, 50, plainSlice...)) // before any keyframe: not cached
	f.video(h264Packet(2, 100, idrSlice...))
	f.video(h264Packet(3, 100, plainSlice...))

	f.add(2, late.events())
	f.video(h264Packet(4, 200, plainSlice...))

	assertSequences(t, "first consumer", first.sequences, 1, 2, 3, 4)
	assertSequences(t, "late joiner", late.sequences, 2, 3, 4)
}

func TestFanoutKeepsTheParameterSetsOfTheKeyframe(t *testing.T) {
	f := newFanout()
	late := &recordedVideo{}

	f.video(h264Packet(1, 100, spsUnit...))
	f.video(h264Packet(2, 100, ppsUnit...))
	f.video(h264Packet(3, 100, idrSlice...)) // same access unit: no restart

	f.add(1, late.events())

	assertSequences(t, "late joiner", late.sequences, 1, 2, 3)
}

func TestFanoutCacheStartsAgainAtTheNextKeyframe(t *testing.T) {
	f := newFanout()
	late := &recordedVideo{}

	f.video(h264Packet(1, 100, idrSlice...))
	f.video(h264Packet(2, 200, plainSlice...))
	f.video(h264Packet(3, 300, fuAIDRStart...))
	f.video(h264Packet(4, 300, fuAIDRMiddle...))

	f.add(1, late.events())

	assertSequences(t, "late joiner", late.sequences, 3, 4)
}

func TestFanoutDropsTheCachePastItsLimit(t *testing.T) {
	f := newFanout()
	late := &recordedVideo{}

	big := make([]byte, keyframeCacheLimit)
	big[0] = 0x41

	f.video(h264Packet(1, 100, idrSlice...))
	f.video(h264Packet(2, 100, big...))
	f.video(h264Packet(3, 200, plainSlice...)) // still waiting for a keyframe

	f.add(1, late.events())

	assertSequences(t, "late joiner", late.sequences)
}

func TestFanoutCachesClonesNotTheReceiverBuffer(t *testing.T) {
	f := newFanout()
	payload := append([]byte(nil), idrSlice...)

	f.video(h264Packet(1, 100, payload...))
	payload[1] = 0xee // the receiver reuses its read buffer

	var replayed []byte

	f.add(1, SourceEvents{VideoRTP: func(packet *rtp.Packet) { replayed = packet.Payload }})

	if len(replayed) != 2 || replayed[1] != 1 {
		t.Fatalf("replayed payload = %v, want the bytes at caching time", replayed)
	}
}

func TestFanoutRemoveCountsWhoIsLeft(t *testing.T) {
	f := newFanout()
	f.add(1, SourceEvents{})
	f.add(2, SourceEvents{})

	if remaining, found := f.remove(1); !found || remaining != 1 {
		t.Fatalf("remove(1) = %d, %t, want 1, true", remaining, found)
	}

	if _, found := f.remove(1); found {
		t.Fatal("remove(1) twice found the consumer again")
	}

	if remaining, found := f.remove(2); !found || remaining != 0 {
		t.Fatalf("remove(2) = %d, %t, want 0, true", remaining, found)
	}
}

func TestFanoutRefusesAJoinOnceClosedAndStillTellsEveryone(t *testing.T) {
	f := newFanout()

	var told error

	f.add(1, SourceEvents{Failed: func(err error) { told = err }})
	f.close()

	if f.add(2, SourceEvents{}) {
		t.Fatal("add() after close() = true, want false")
	}

	cause := errors.New("ended")
	f.failed(cause)

	if !errors.Is(told, cause) {
		t.Fatalf("consumer told %v, want %v", told, cause)
	}
}

func TestFanoutConsumerMayLeaveFromItsOwnCallback(t *testing.T) {
	f := newFanout()
	f.add(1, SourceEvents{VideoRTP: func(*rtp.Packet) { f.remove(1) }})

	f.video(h264Packet(1, 100, plainSlice...)) // must not deadlock

	if remaining, _ := f.remove(1); remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
}

package media

import (
	"testing"

	"bticino-go-companion/internal/core"
	"github.com/pion/rtp"
)

func TestDistributor_Distribute(t *testing.T) {
	distributor := NewDistributor()
	source := testSource()
	if err := distributor.RegisterSource(source); err != nil {
		t.Fatalf("RegisterSource() error = %v", err)
	}

	first := make(chan Packet, 1)
	second := make(chan Packet, 1)
	if err := distributor.RegisterConsumer("first", ConsumerFunc(func(packet Packet) {
		first <- packet
	})); err != nil {
		t.Fatalf("RegisterConsumer() first error = %v", err)
	}
	if err := distributor.RegisterConsumer("second", ConsumerFunc(func(packet Packet) {
		second <- packet
	})); err != nil {
		t.Fatalf("RegisterConsumer() second error = %v", err)
	}

	rtpPacket := testRTPPacket(source.SSRC)
	if !distributor.Distribute(source, rtpPacket) {
		t.Fatal("Distribute() = false, want true")
	}

	firstPacket := <-first
	secondPacket := <-second
	if firstPacket.Source != source {
		t.Errorf("first packet source = %#v, want %#v", firstPacket.Source, source)
	}
	if secondPacket.Source != source {
		t.Errorf("second packet source = %#v, want %#v", secondPacket.Source, source)
	}
	if firstPacket.RTP == rtpPacket || secondPacket.RTP == rtpPacket || firstPacket.RTP == secondPacket.RTP {
		t.Fatal("consumers received shared RTP packets")
	}
	firstPacket.RTP.Payload[0] = 99
	if rtpPacket.Payload[0] == 99 || secondPacket.RTP.Payload[0] == 99 {
		t.Fatal("consumer RTP payload mutation escaped its copy")
	}
}

func TestDistributor_DistributeRejectsUnmatchedSource(t *testing.T) {
	tests := []struct {
		name   string
		source Source
		ssrc   uint32
	}{
		{
			name: "unregistered entrypoint",
			source: Source{
				EntrypointID: "other",
				MediaKind:    MediaKindVideo,
				SSRC:         1234,
				Generation:   "session-1",
			},
			ssrc: 1234,
		},
		{
			name: "unmatched generation",
			source: Source{
				EntrypointID: "front-door",
				MediaKind:    MediaKindVideo,
				SSRC:         1234,
				Generation:   "session-2",
			},
			ssrc: 1234,
		},
		{
			name: "unmatched ssrc in source",
			source: Source{
				EntrypointID: "front-door",
				MediaKind:    MediaKindVideo,
				SSRC:         5678,
				Generation:   "session-1",
			},
			ssrc: 5678,
		},
		{
			name:   "unmatched ssrc in packet",
			source: testSource(),
			ssrc:   5678,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			distributor := NewDistributor()
			registered := testSource()
			if err := distributor.RegisterSource(registered); err != nil {
				t.Fatalf("RegisterSource() error = %v", err)
			}

			received := make(chan Packet, 1)
			if err := distributor.RegisterConsumer("consumer", ConsumerFunc(func(packet Packet) {
				received <- packet
			})); err != nil {
				t.Fatalf("RegisterConsumer() error = %v", err)
			}

			if distributor.Distribute(test.source, testRTPPacket(test.ssrc)) {
				t.Fatal("Distribute() = true, want false")
			}
			select {
			case packet := <-received:
				t.Fatalf("consumer received packet %#v", packet)
			default:
			}
		})
	}
}

func TestDistributor_RegisterSourceReplacesGeneration(t *testing.T) {
	distributor := NewDistributor()
	first := testSource()
	second := first
	second.Generation = "session-2"
	second.SSRC = 5678
	if err := distributor.RegisterSource(first); err != nil {
		t.Fatalf("RegisterSource() first error = %v", err)
	}
	if err := distributor.RegisterSource(second); err != nil {
		t.Fatalf("RegisterSource() second error = %v", err)
	}

	received := make(chan Packet, 1)
	if err := distributor.RegisterConsumer("consumer", ConsumerFunc(func(packet Packet) {
		received <- packet
	})); err != nil {
		t.Fatalf("RegisterConsumer() error = %v", err)
	}

	if distributor.Distribute(first, testRTPPacket(first.SSRC)) {
		t.Fatal("Distribute() old source = true, want false")
	}
	if !distributor.Distribute(second, testRTPPacket(second.SSRC)) {
		t.Fatal("Distribute() current source = false, want true")
	}
	<-received
	if distributor.UnregisterSource(first) {
		t.Fatal("UnregisterSource() old source = true, want false")
	}
	if !distributor.UnregisterSource(second) {
		t.Fatal("UnregisterSource() current source = false, want true")
	}
}

func TestDistributor_UnregisterConsumer(t *testing.T) {
	distributor := NewDistributor()
	source := testSource()
	if err := distributor.RegisterSource(source); err != nil {
		t.Fatalf("RegisterSource() error = %v", err)
	}

	received := make(chan Packet, 1)
	if err := distributor.RegisterConsumer("consumer", ConsumerFunc(func(packet Packet) {
		received <- packet
	})); err != nil {
		t.Fatalf("RegisterConsumer() error = %v", err)
	}
	if !distributor.UnregisterConsumer("consumer") {
		t.Fatal("UnregisterConsumer() = false, want true")
	}
	if distributor.UnregisterConsumer("consumer") {
		t.Fatal("UnregisterConsumer() second call = true, want false")
	}

	if !distributor.Distribute(source, testRTPPacket(source.SSRC)) {
		t.Fatal("Distribute() = false, want true")
	}
	select {
	case packet := <-received:
		t.Fatalf("unregistered consumer received packet %#v", packet)
	default:
	}
}

func TestDistributor_RegisterRejectsInvalidValues(t *testing.T) {
	distributor := NewDistributor()
	if err := distributor.RegisterSource(Source{}); err != ErrInvalidSource {
		t.Errorf("RegisterSource() error = %v, want %v", err, ErrInvalidSource)
	}
	if err := distributor.RegisterConsumer("", ConsumerFunc(func(Packet) {})); err != ErrInvalidConsumerID {
		t.Errorf("RegisterConsumer() empty ID error = %v, want %v", err, ErrInvalidConsumerID)
	}
	if err := distributor.RegisterConsumer("consumer", nil); err != ErrNilConsumer {
		t.Errorf("RegisterConsumer() nil consumer error = %v, want %v", err, ErrNilConsumer)
	}
}

func testSource() Source {
	return Source{
		EntrypointID: core.EntrypointID("front-door"),
		MediaKind:    MediaKindVideo,
		SSRC:         1234,
		Generation:   "session-1",
	}
}

func testRTPPacket(ssrc uint32) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: 1,
			Timestamp:      2,
			SSRC:           ssrc,
		},
		Payload: []byte{1, 2, 3},
	}
}

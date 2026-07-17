package media

import (
	"testing"
)

func TestDistributor_RegisterSessionConsumerScopesPackets(t *testing.T) {
	t.Parallel()

	distributor := NewDistributor()
	video := testSource()
	audio := video
	audio.MediaKind = MediaKindAudio
	audio.SSRC = 5678

	if err := distributor.RegisterSource(video); err != nil {
		t.Fatal(err)
	}

	if err := distributor.RegisterSource(audio); err != nil {
		t.Fatal(err)
	}

	received := make(chan Packet, 1)

	if err := distributor.RegisterSessionConsumer(video, "video-session", ConsumerFunc(func(packet Packet) {
		received <- packet
	})); err != nil {
		t.Fatal(err)
	}

	if !distributor.Distribute(video, testRTPPacket(video.SSRC)) {
		t.Fatal("Distribute() = false, want true")
	}

	<-received

	if !distributor.Distribute(audio, testRTPPacket(audio.SSRC)) {
		t.Fatal("Distribute() = false, want true")
	}

	select {
	case packet := <-received:
		t.Fatalf("received packet for wrong source: %#v", packet)
	default:
	}
}

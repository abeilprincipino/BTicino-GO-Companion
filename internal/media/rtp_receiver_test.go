package media

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestRTPReceiverParsesExpectedPayloadAndReportsFlow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received := make(chan *rtp.Packet, 1)
	receiver := NewRTPReceiver(RTPReceiverConfig{Address: "127.0.0.1:0", Codec: "H264", PayloadType: VideoPayloadType, Packet: func(packet *rtp.Packet) { received <- packet }})
	if err := receiver.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	packet := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: VideoPayloadType, SequenceNumber: 7, SSRC: 42}, Payload: []byte{1, 2, 3}}
	data, err := packet.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("udp", receiver.Metadata().LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got.SSRC != 42 || got.PayloadType != VideoPayloadType {
			t.Fatalf("packet = %#v", got.Header)
		}
	case <-time.After(time.Second):
		t.Fatal("receiver did not receive RTP")
	}
	metadata := receiver.Metadata()
	if metadata.Codec != "H264" || metadata.PacketCount != 1 || metadata.SSRC != 42 || !receiver.RecentlyFlowing(time.Second) {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestRTPReceiverRejectsUnexpectedPayloadType(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receiver := NewRTPReceiver(RTPReceiverConfig{Address: "127.0.0.1:0", Codec: "Speex/8000", PayloadType: AudioPayloadType})
	if err := receiver.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	packet := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: VideoPayloadType, SSRC: 42}}
	data, _ := packet.Marshal()
	conn, err := net.Dial("udp", receiver.Metadata().LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write(data)
	time.Sleep(20 * time.Millisecond)
	metadata := receiver.Metadata()
	if metadata.PacketCount != 0 || metadata.InvalidCount != 1 || receiver.RecentlyFlowing(time.Second) {
		t.Fatalf("metadata = %#v", metadata)
	}
}

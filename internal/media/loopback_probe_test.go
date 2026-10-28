package media

import (
	"testing"
	"time"
)

func TestUDPPayloadReadsPortAndPayloadOfAnIPv4Datagram(t *testing.T) {
	packet := make([]byte, 20+8+3)
	packet[0] = 0x45                        // IPv4, 20-byte header
	packet[9] = 17                          // UDP
	packet[20+2], packet[20+3] = 0x13, 0x8a // destination port 5002
	copy(packet[28:], []byte{1, 2, 3})

	port, payload, ok := udpPayload(packet)
	if !ok || port != 5002 || string(payload) != string([]byte{1, 2, 3}) {
		t.Fatalf("udpPayload = %d %v %v, want 5002 [1 2 3] true", port, payload, ok)
	}

	packet[9] = 6 // TCP
	if _, _, ok := udpPayload(packet); ok {
		t.Fatal("udpPayload accepted a TCP segment")
	}
}

func TestLoopbackStreamRecordsParameterSetsAndKeyframe(t *testing.T) {
	stream := &loopbackStream{}

	stream.observe([]byte{0x41, 0}, 10*time.Millisecond)                            // non-IDR slice
	stream.observe([]byte{0x18, 0, 2, 0x67, 0, 0, 2, 0x68, 0}, 20*time.Millisecond) // STAP-A: SPS, PPS
	stream.observe([]byte{0x7c, 0x85, 0}, 30*time.Millisecond)                      // FU-A start of an IDR
	stream.observe([]byte{0x7c, 0x05, 0}, 40*time.Millisecond)                      // FU-A continuation

	if !stream.sawSPS || stream.firstSPS != 20*time.Millisecond {
		t.Fatalf("first SPS = %v %v, want at 20ms", stream.sawSPS, stream.firstSPS)
	}

	if !stream.sawIDR || stream.firstIDR != 30*time.Millisecond {
		t.Fatalf("first IDR = %v %v, want at 30ms", stream.sawIDR, stream.firstIDR)
	}

	if got := len(stream.nalTypes); got != 4 {
		t.Fatalf("nal types = %v, want 1 7 8 5", stream.nalTypes)
	}
}

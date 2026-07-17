package media

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestUDPBackchannel_WriteRTP(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	backchannel, err := NewUDPBackchannel(listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer backchannel.Close()

	want := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    backchannelSpeexPayloadType,
			SequenceNumber: 42,
			Timestamp:      1234,
			SSRC:           5678,
		},
		Payload: []byte{1, 2, 3},
	}
	if err := backchannel.WriteRTP(want); err != nil {
		t.Fatalf("WriteRTP() error = %v", err)
	}
	wantRaw, err := want.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1500)
	n, _, err := listener.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(buffer[:n], wantRaw) {
		t.Errorf("received datagram = %x, want %x", buffer[:n], wantRaw)
	}
}

func TestUDPBackchannel_WriteRTPRejectsInvalidPacket(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	backchannel, err := NewUDPBackchannel(listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer backchannel.Close()

	for _, packet := range []*rtp.Packet{nil, {Header: rtp.Header{PayloadType: 96}}} {
		if err := backchannel.WriteRTP(packet); !errors.Is(err, ErrInvalidBackchannelPacket) {
			t.Errorf("WriteRTP(%#v) error = %v, want %v", packet, err, ErrInvalidBackchannelPacket)
		}
	}
}

func TestNewUDPBackchannel_DefaultAddress(t *testing.T) {
	backchannel, err := NewUDPBackchannel("")
	if err != nil {
		t.Fatal(err)
	}
	defer backchannel.Close()

	if got := backchannel.conn.RemoteAddr().String(); got != DefaultBackchannelAddress {
		t.Errorf("remote address = %q, want %q", got, DefaultBackchannelAddress)
	}
}

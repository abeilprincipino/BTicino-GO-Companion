package media

import (
	"errors"
	"fmt"
	"net"

	"github.com/pion/rtp"
)

var ErrInvalidBackchannelPacket = errors.New("media: invalid backchannel packet")

const (
	DefaultBackchannelAddress   = "127.0.0.1:4000"
	backchannelSpeexPayloadType = 97
)

type Backchannel interface {
	WriteRTP(*rtp.Packet) error
}

type BackchannelFunc func(*rtp.Packet) error

func (f BackchannelFunc) WriteRTP(packet *rtp.Packet) error {
	return f(packet)
}

// UDPBackchannel sends Speex RTP packets to the intercom backchannel.
type UDPBackchannel struct {
	conn *net.UDPConn
}

func NewUDPBackchannel(address string) (*UDPBackchannel, error) {
	if address == "" {
		address = DefaultBackchannelAddress
	}

	remote, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, fmt.Errorf("resolve backchannel address: %w", err)
	}

	conn, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		return nil, fmt.Errorf("dial backchannel: %w", err)
	}

	return &UDPBackchannel{conn: conn}, nil
}

func (b *UDPBackchannel) WriteRTP(packet *rtp.Packet) error {
	if packet == nil || packet.PayloadType != backchannelSpeexPayloadType {
		return ErrInvalidBackchannelPacket
	}

	raw, err := packet.Marshal()
	if err != nil {
		return fmt.Errorf("marshal backchannel RTP: %w", err)
	}

	if _, err := b.conn.Write(raw); err != nil {
		return fmt.Errorf("send backchannel RTP: %w", err)
	}

	return nil
}

func (b *UDPBackchannel) Close() error {
	return b.conn.Close()
}

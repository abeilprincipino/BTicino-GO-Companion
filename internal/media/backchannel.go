package media

import (
	"errors"

	"github.com/pion/rtp"
)

var ErrInvalidBackchannelPacket = errors.New("media: invalid backchannel packet")

type Backchannel interface {
	WriteRTP(*rtp.Packet) error
}

type BackchannelFunc func(*rtp.Packet) error

func (f BackchannelFunc) WriteRTP(packet *rtp.Packet) error {
	return f(packet)
}

package media

import (
	"errors"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"
)

var ErrRTSPConsumerClosed = errors.New("media: RTSP consumer is closed")

type RTPWriter interface {
	WriteRTP(*rtp.Packet) error
}

type RTSPConsumer struct {
	mu sync.Mutex

	distributor *Distributor
	source      Source
	sessionID   SessionID
	writer      RTPWriter
	closed      bool
}

func NewRTSPConsumer(distributor *Distributor, source Source, sessionID SessionID, writer RTPWriter) (*RTSPConsumer, error) {
	if writer == nil {
		return nil, ErrNilConsumer
	}
	consumer := &RTSPConsumer{
		distributor: distributor,
		source:      source,
		sessionID:   sessionID,
		writer:      writer,
	}
	if distributor == nil {
		return nil, ErrSourceNotRegistered
	}
	if err := distributor.RegisterSessionConsumer(source, sessionID, consumer); err != nil {
		return nil, err
	}
	return consumer, nil
}

func (c *RTSPConsumer) Consume(packet Packet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	_ = c.writer.WriteRTP(packet.RTP)
}

func (c *RTSPConsumer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrRTSPConsumerClosed
	}
	c.closed = true
	c.distributor.UnregisterSessionConsumer(c.source, c.sessionID)
	return nil
}

type RTSPPacketWriter struct {
	session *gortsplib.ServerSession
	media   *description.Media
}

func NewRTSPPacketWriter(session *gortsplib.ServerSession, media *description.Media) *RTSPPacketWriter {
	return &RTSPPacketWriter{session: session, media: media}
}

func (w *RTSPPacketWriter) WriteRTP(packet *rtp.Packet) error {
	if w == nil || w.session == nil || w.media == nil || packet == nil {
		return ErrInvalidBackchannelPacket
	}
	return w.session.WritePacketRTP(w.media, packet)
}

type RTSPBackchannel struct {
	writer RTPWriter
}

func NewRTSPBackchannel(writer RTPWriter) *RTSPBackchannel {
	return &RTSPBackchannel{writer: writer}
}

func (b *RTSPBackchannel) WriteRTP(packet *rtp.Packet) error {
	if packet == nil || b == nil || b.writer == nil {
		return ErrInvalidBackchannelPacket
	}
	return b.writer.WriteRTP(packet)
}

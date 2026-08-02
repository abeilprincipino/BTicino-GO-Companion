package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pion/rtp"
)

const (
	VideoPayloadType    = 96
	AudioPayloadType    = 110
	packetStatsInterval = 30 * time.Second
	invalidLogInterval  = 10 * time.Second
)

var ErrRTPReceiverStarted = errors.New("media: rtp receiver already started")

type RTPMetadata struct {
	Codec        string
	PayloadType  uint8
	SSRC         uint32
	PacketCount  uint64
	InvalidCount uint64
	LastPacketAt time.Time
	LocalAddr    string
	LocalPort    int
}

type RTPReceiverConfig struct {
	Address     string
	Codec       string
	PayloadType uint8
	Logger      *slog.Logger
	Packet      func(*rtp.Packet)
}

// RTPReceiver owns one UDP socket until its context is cancelled or Close runs.
type RTPReceiver struct {
	mu              sync.RWMutex
	address         string
	codec           string
	payloadType     uint8
	logger          *slog.Logger
	packet          func(*rtp.Packet)
	conn            *net.UDPConn
	done            chan struct{}
	metadata        RTPMetadata
	lastInvalidLog  time.Time
	invalidSinceLog uint64
}

func NewVideoRTPReceiver(logger *slog.Logger, packet func(*rtp.Packet)) *RTPReceiver {
	return NewRTPReceiver(RTPReceiverConfig{Address: "127.0.0.1:0", Codec: "H264", PayloadType: VideoPayloadType, Logger: logger, Packet: packet})
}

func NewAudioRTPReceiver(logger *slog.Logger, packet func(*rtp.Packet)) *RTPReceiver {
	return NewRTPReceiver(RTPReceiverConfig{Address: "127.0.0.1:0", Codec: "Speex/8000", PayloadType: AudioPayloadType, Logger: logger, Packet: packet})
}

func NewRTPReceiver(cfg RTPReceiverConfig) *RTPReceiver {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &RTPReceiver{address: cfg.Address, codec: cfg.Codec, payloadType: cfg.PayloadType, logger: logger.With("component", "media.rtp", "direction", "downlink", "codec", cfg.Codec, "payload_type", cfg.PayloadType), packet: cfg.Packet}
}

func (r *RTPReceiver) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.conn != nil {
		return ErrRTPReceiverStarted
	}

	addr, err := net.ResolveUDPAddr("udp4", r.address)
	if err != nil {
		return fmt.Errorf("resolve rtp address: %w", err)
	}

	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fmt.Errorf("listen rtp: %w", err)
	}

	r.conn = conn
	r.done = make(chan struct{})
	r.metadata = RTPMetadata{Codec: r.codec, PayloadType: r.payloadType, LocalAddr: conn.LocalAddr().String(), LocalPort: conn.LocalAddr().(*net.UDPAddr).Port}

	r.logger.InfoContext(ctx, "rtp receiver bound", "local_addr", r.metadata.LocalAddr)
	go func(done <-chan struct{}) {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}(r.done)
	go r.read(ctx, conn, r.done)
	go r.logPacketStatsPeriodically(r.done)

	return nil
}

func (r *RTPReceiver) Close() error {
	r.mu.Lock()

	conn, done := r.conn, r.done
	if conn == nil {
		r.mu.Unlock()
		return nil
	}

	r.conn, r.done = nil, nil
	r.mu.Unlock()

	err := conn.Close()

	<-done

	return err
}

func (r *RTPReceiver) RecentlyFlowing(window time.Duration) bool {
	r.mu.RLock()
	last := r.metadata.LastPacketAt
	r.mu.RUnlock()

	return !last.IsZero() && time.Since(last) <= window
}

func (r *RTPReceiver) Metadata() RTPMetadata {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.metadata
}

func (r *RTPReceiver) read(ctx context.Context, conn *net.UDPConn, done chan struct{}) {
	defer close(done)

	buf := make([]byte, 65535)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}

			r.logger.Warn("read rtp", "error", err)

			continue
		}

		packet := &rtp.Packet{}
		if err := packet.Unmarshal(buf[:n]); err != nil || packet.PayloadType != r.payloadType {
			r.recordInvalidPacket()
			continue
		}

		r.mu.Lock()
		r.metadata.PacketCount++
		r.metadata.SSRC = packet.SSRC
		r.metadata.LastPacketAt = time.Now()
		firstPacket := r.metadata.PacketCount == 1
		localAddr := r.metadata.LocalAddr
		r.mu.Unlock()

		if firstPacket {
			r.logger.Info("rtp receiver received first valid packet", "ssrc", packet.SSRC, "local_addr", localAddr)
		}

		if r.packet != nil {
			r.packet(packet)
		}
	}
}

func (r *RTPReceiver) recordInvalidPacket() {
	now := time.Now()

	r.mu.Lock()
	r.metadata.InvalidCount++
	r.invalidSinceLog++
	invalidCount := r.metadata.InvalidCount
	invalidSinceLog := r.invalidSinceLog

	shouldLog := r.lastInvalidLog.IsZero() || now.Sub(r.lastInvalidLog) >= invalidLogInterval
	if shouldLog {
		r.lastInvalidLog = now
		r.invalidSinceLog = 0
	}
	r.mu.Unlock()

	if shouldLog {
		r.logger.Warn("invalid rtp packets received", "invalid_count", invalidCount, "invalid_since_last_log", invalidSinceLog)
	}
}

func (r *RTPReceiver) logPacketStats() {
	metadata := r.Metadata()
	r.logger.Debug("rtp packet stats", "packet_count", metadata.PacketCount, "invalid_count", metadata.InvalidCount, "ssrc", metadata.SSRC, "local_addr", metadata.LocalAddr)
}

func (r *RTPReceiver) logPacketStatsPeriodically(done <-chan struct{}) {
	ticker := time.NewTicker(packetStatsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			r.logPacketStats()
		}
	}
}

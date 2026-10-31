package signaling

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

const (
	// mediaKeepaliveInterval is how often an established dialog sends one RTP
	// packet towards the far end's audio address.
	mediaKeepaliveInterval = 5 * time.Second

	// keepalivePayloadType and keepaliveClockRate match the audio line of our
	// own SDP (mediaLines: "a=rtpmap:110 speex/8000"), so the packet belongs to
	// the stream the far end negotiated with us.
	keepalivePayloadType = 110
	keepaliveClockRate   = 8000
)

// keepalivePayload is the body of every keepalive packet: one byte, which is
// enough for the far end to count the stream as alive without decoding audio.
// It is also what ha-bticino-c300x sends to the intercom as Speex silence.
var keepalivePayload = []byte{0}

// The companion never sends audio upstream: the clear RTP it consumes arrives
// through OpenWebNet AV requests, not through the SIP media it advertised. A
// far end that watches its RTP for inactivity therefore sees a silent stream
// and ends the answered call on its own. rtpKeepalive closes that gap by
// sending a minimal RTP packet on the advertised audio port, at a fixed
// interval, for as long as the dialog lasts. When both SDPs negotiated SRTP
// the packet is encrypted with our key, or the far end could not authenticate
// it and would not count it.
type rtpKeepalive struct {
	conn     *net.UDPConn
	remote   *net.UDPAddr
	srtp     *srtp.Context
	interval time.Duration
	logger   *slog.Logger
	packet   rtp.Packet
	step     uint32
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// startRTPKeepalive binds localPort — the audio port our SDP advertised, so the
// packets come from where the far end expects them — and starts sending to
// remote every interval until Stop is called, through protect when it is set.
func startRTPKeepalive(localPort int, remote *net.UDPAddr, protect *srtp.Context, interval time.Duration, logger *slog.Logger) (*rtpKeepalive, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("media keepalive interval must be positive, got %s", interval)
	}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: localPort})
	if err != nil {
		return nil, fmt.Errorf("bind media keepalive port %d: %w", localPort, err)
	}

	if _, ok := conn.LocalAddr().(*net.UDPAddr); !ok {
		_ = conn.Close()

		return nil, fmt.Errorf("bind media keepalive port %d: no local address", localPort)
	}

	k := &rtpKeepalive{
		conn:     conn,
		remote:   remote,
		srtp:     protect,
		interval: interval,
		logger:   logger,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}

	go k.run()

	return k, nil
}

// run sends the first packet at once and then one per tick. A failed write is
// logged once, not every interval: the far end being unreachable is one fact,
// not a stream of them.
func (k *rtpKeepalive) run() {
	defer close(k.done)
	defer func() { _ = k.conn.Close() }()

	k.step = uint32(k.interval.Milliseconds() * keepaliveClockRate / 1000)
	k.packet = rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    keepalivePayloadType,
			SequenceNumber: uint16(randomUint(1 << 16)),
			Timestamp:      uint32(randomUint(1 << 32)),
			SSRC:           uint32(randomUint(1 << 32)),
		},
		Payload: keepalivePayload,
	}

	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()

	warned := false

	for {
		if err := k.write(); err != nil && !warned {
			warned = true
			k.logger.Warn("sip media keepalive write failed", "remote", k.remote.String(), "error", err)
		}

		select {
		case <-k.stop:
			return
		case <-ticker.C:
		}
	}
}

// write sends the current packet and advances it, so consecutive packets carry
// the sequence numbers and timestamps of a continuous stream.
func (k *rtpKeepalive) write() error {
	data, err := k.packet.Marshal()
	if err != nil {
		return fmt.Errorf("marshal keepalive RTP: %w", err)
	}

	if k.srtp != nil {
		if data, err = k.srtp.EncryptRTP(nil, data, nil); err != nil {
			return fmt.Errorf("encrypt keepalive RTP: %w", err)
		}
	}

	if _, err := k.conn.WriteToUDP(data, k.remote); err != nil {
		return fmt.Errorf("send keepalive RTP: %w", err)
	}

	k.packet.SequenceNumber++
	k.packet.Timestamp += k.step

	return nil
}

// Stop ends the keepalive and returns once its socket is closed. It is safe to
// call more than once.
func (k *rtpKeepalive) Stop() {
	k.stopOnce.Do(func() { close(k.stop) })
	<-k.done
}

// startDialogKeepalive keeps the media of one established dialog alive until
// ctx — the dialog's own context — ends. Without a usable audio target in the
// far end's SDP there is nothing to keep alive, and the call goes on without
// it: a keepalive is never a reason to fail a dialog. localSDP is the one we
// sent, whose audio key encrypts the keepalive when the far end's SDP accepted
// SRTP too.
func startDialogKeepalive(ctx context.Context, logger *slog.Logger, remoteSDP, localSDP string, localPort int, interval time.Duration) {
	remote, err := parseRemoteAudioTarget(remoteSDP)
	if err != nil {
		logger.Warn("sip media keepalive unavailable", "error", err)

		return
	}

	protect, err := keepaliveSRTP(remoteSDP, localSDP)
	if err != nil {
		logger.Warn("sip media keepalive unavailable", "error", err)

		return
	}

	keepalive, err := startRTPKeepalive(localPort, remote, protect, interval, logger)
	if err != nil {
		logger.Warn("sip media keepalive unavailable", "remote", remote.String(), "error", err)

		return
	}

	go func() {
		select {
		case <-ctx.Done():
		case <-keepalive.done:
		}

		keepalive.Stop()
	}()

	logger.Debug("sip media keepalive started",
		"remote", remote.String(),
		"local_port", localPort,
		"interval", interval,
		"srtp", protect != nil)
}

// keepaliveSRTP returns the context that encrypts the keepalive, nil when the
// far end's audio carries no SRTP key and so expects plain RTP.
func keepaliveSRTP(remoteSDP, localSDP string) (*srtp.Context, error) {
	if _, ok := parseAudioCrypto(remoteSDP); !ok {
		return nil, nil
	}

	key, ok := parseAudioCrypto(localSDP)
	if !ok {
		return nil, errors.New("the far end negotiated SRTP but our SDP has no audio key")
	}

	context, err := srtp.CreateContext(key[:16], key[16:], srtp.ProtectionProfileAes128CmHmacSha1_80)
	if err != nil {
		return nil, fmt.Errorf("create keepalive SRTP context: %w", err)
	}

	return context, nil
}

// randomUint returns a uniform value in [0, limit), or 0 if the system's
// random source fails — an RTP start value only has to be unpredictable, not
// guaranteed.
func randomUint(limit int64) uint64 {
	n, err := rand.Int(rand.Reader, big.NewInt(limit))
	if err != nil {
		return 0
	}

	return n.Uint64()
}

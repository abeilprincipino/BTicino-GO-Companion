package media

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/rtp"
)

// loopbackProbeRunning keeps one probe at a time: a ring that repeats while a
// probe runs is already covered by it.
var loopbackProbeRunning atomic.Bool

const (
	loopbackProbePayloadType = 96
	loopbackProbeMaxStreams  = 16
	loopbackProbeMaxNALTypes = 16
)

// loopbackStream is what the probe saw of one RTP stream, by destination port.
type loopbackStream struct {
	port           int
	ssrc           uint32
	packets        int
	first          time.Duration
	firstSPS       time.Duration
	firstIDR       time.Duration
	sawSPS, sawIDR bool
	nalTypes       []string
}

// ProbeLoopbackStreams logs, for every H.264 RTP stream sent over the loopback
// interface during window, when its first packet, parameter sets and keyframe
// passed relative to the call. It reads copies of the packets from a packet
// socket, so the processes the streams are sent to see no difference. It is a
// diagnostic: it tells whether the indoor unit streams video to its own
// display before it accepts the companion's request.
func ProbeLoopbackStreams(ctx context.Context, logger *slog.Logger, window time.Duration) {
	if !loopbackProbeRunning.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer loopbackProbeRunning.Store(false)

		streams, err := probeLoopback(ctx, window)
		if err != nil {
			logger.Debug("loopback stream probe failed", "error", err)
			return
		}

		if len(streams) == 0 {
			logger.Debug("loopback stream probe saw no video stream", "window", window)
		}

		for _, s := range streams {
			logger.Debug("loopback video stream", "port", s.port, "ssrc", s.ssrc, "packets", s.packets,
				"first_packet", s.first, "first_sps", optionalOffset(s.sawSPS, s.firstSPS), "first_idr", optionalOffset(s.sawIDR, s.firstIDR),
				"first_nal_types", strings.Join(s.nalTypes, " "))
		}
	}()
}

func optionalOffset(seen bool, offset time.Duration) string {
	if !seen {
		return "none"
	}

	return offset.String()
}

func probeLoopback(ctx context.Context, window time.Duration) ([]*loopbackStream, error) {
	iface, err := net.InterfaceByName("lo")
	if err != nil {
		return nil, err
	}

	protocol := htons(syscall.ETH_P_IP)

	// SOCK_DGRAM drops the link-layer header: every read starts at the IP header.
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_DGRAM, int(protocol))
	if err != nil {
		return nil, fmt.Errorf("open packet socket: %w", err)
	}
	defer syscall.Close(fd)

	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: protocol, Ifindex: iface.Index}); err != nil {
		return nil, fmt.Errorf("bind packet socket to lo: %w", err)
	}

	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)

	// A read timeout lets the loop notice the end of the window on a quiet link.
	timeout := syscall.NsecToTimeval((100 * time.Millisecond).Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeout); err != nil {
		return nil, fmt.Errorf("set packet socket timeout: %w", err)
	}

	start := time.Now()
	deadline := start.Add(window)
	streams := map[int]*loopbackStream{}
	buf := make([]byte, 65536)

	for time.Now().Before(deadline) && ctx.Err() == nil {
		n, from, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			continue // timeout or interrupted read
		}

		// lo hands every packet over twice: keep the received copy.
		if link, ok := from.(*syscall.SockaddrLinklayer); ok && link.Pkttype == syscall.PACKET_OUTGOING {
			continue
		}

		port, payload, ok := udpPayload(buf[:n])
		if !ok {
			continue
		}

		var packet rtp.Packet
		if packet.Unmarshal(payload) != nil || packet.PayloadType != loopbackProbePayloadType || len(packet.Payload) == 0 {
			continue
		}

		stream := streams[port]
		if stream == nil {
			if len(streams) >= loopbackProbeMaxStreams {
				continue
			}

			stream = &loopbackStream{port: port, ssrc: packet.SSRC, first: time.Since(start)}
			streams[port] = stream
		}

		stream.packets++
		stream.observe(packet.Payload, time.Since(start))
	}

	result := make([]*loopbackStream, 0, len(streams))
	for _, s := range streams {
		result = append(result, s)
	}

	sort.Slice(result, func(i, j int) bool { return result[i].first < result[j].first })

	return result, nil
}

// observe records the NAL units a packet starts, by RFC 6184 payload type.
func (s *loopbackStream) observe(payload []byte, at time.Duration) {
	var types []byte

	switch nal := payload[0] & 0x1f; nal {
	case 24: // STAP-A: 16-bit size, then the NAL unit
		for rest := payload[1:]; len(rest) > 2; {
			size := int(rest[0])<<8 | int(rest[1])
			if size == 0 || len(rest) < 2+size {
				break
			}

			types = append(types, rest[2]&0x1f)
			rest = rest[2+size:]
		}
	case 28: // FU-A: only the fragment that starts the NAL unit
		if len(payload) > 1 && payload[1]&0x80 != 0 {
			types = append(types, payload[1]&0x1f)
		}
	default:
		types = append(types, nal)
	}

	for _, t := range types {
		switch t {
		case 7:
			if !s.sawSPS {
				s.sawSPS, s.firstSPS = true, at
			}
		case 5:
			if !s.sawIDR {
				s.sawIDR, s.firstIDR = true, at
			}
		}

		if len(s.nalTypes) < loopbackProbeMaxNALTypes {
			s.nalTypes = append(s.nalTypes, fmt.Sprint(t))
		}
	}
}

// udpPayload returns the destination port and payload of an IPv4 UDP datagram.
func udpPayload(packet []byte) (int, []byte, bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 || packet[9] != syscall.IPPROTO_UDP {
		return 0, nil, false
	}

	// A later fragment carries no UDP header.
	if (int(packet[6]&0x1f)<<8 | int(packet[7])) != 0 {
		return 0, nil, false
	}

	headerLength := int(packet[0]&0x0f) * 4
	if len(packet) < headerLength+8 {
		return 0, nil, false
	}

	udp := packet[headerLength:]

	return int(udp[2])<<8 | int(udp[3]), udp[8:], true
}

func htons(v uint16) uint16 {
	return v<<8 | v>>8
}

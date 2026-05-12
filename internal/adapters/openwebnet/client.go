package openwebnet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"bticino-go-companion/internal/config"
	openwebnetproto "bticino-go-companion/internal/protocol/openwebnet"
)

var (
	ErrHandshakeFailed      = errors.New("openwebnet handshake failed")
	ErrAuthenticationNeeded = errors.New("openwebnet authentication required")
	ErrUnexpectedReply      = errors.New("openwebnet unexpected reply")
)

const (
	ownAckFrame  = "*#*1##"
	ownNackFrame = "*#*0##"
)

var frameRegexp = regexp.MustCompile(`\*#?.*?##`)

type CommandClient struct {
	host        string
	port        int
	timeout     time.Duration
	password    string
	unlockDelay time.Duration
	traceSink   func(string, map[string]any)
}

type frameReader struct {
	conn    net.Conn
	pending string
}

func NewCommandClient(cfg config.Config) *CommandClient {
	timeout := time.Duration(cfg.OpenWebNetCommandSec) * time.Second
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &CommandClient{
		host:        strings.TrimSpace(cfg.OpenWebNetCommandHost),
		port:        cfg.OpenWebNetCommandPort,
		timeout:     timeout,
		password:    strings.TrimSpace(cfg.OpenWebNetCommandPassword),
		unlockDelay: 2 * time.Second,
	}
}

func (c *CommandClient) SetTraceSink(sink func(string, map[string]any)) {
	c.traceSink = sink
}

func (c *CommandClient) Unlock(ctx context.Context, devAddr string) error {
	if strings.TrimSpace(devAddr) == "" {
		return errors.New("empty devaddr")
	}
	return c.exec(ctx, func(reader *frameReader) error {
		if err := c.sendAndExpectSuccess(reader, "lock.unlock.open", openwebnetproto.BuildUnlockOpen(devAddr)); err != nil {
			return fmt.Errorf("unlock open: %w", err)
		}
		timer := time.NewTimer(c.unlockDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if err := c.sendAndExpectSuccess(reader, "lock.unlock.close", openwebnetproto.BuildUnlockClose(devAddr)); err != nil {
			return fmt.Errorf("unlock close: %w", err)
		}
		return nil
	})
}

func (c *CommandClient) StreamStart(ctx context.Context, audioPort, videoPort int) error {
	if audioPort <= 0 || videoPort <= 0 {
		return errors.New("invalid stream ports")
	}
	return c.exec(ctx, func(reader *frameReader) error {
		if err := c.sendAndExpectSuccess(reader, "stream.start.video", openwebnetproto.BuildStreamStartVideo(videoPort)); err != nil {
			return fmt.Errorf("stream start video: %w", err)
		}
		if err := c.sendAndExpectSuccess(reader, "stream.start.audio", openwebnetproto.BuildStreamStartAudio(audioPort)); err != nil {
			return fmt.Errorf("stream start audio: %w", err)
		}
		return nil
	})
}

func (c *CommandClient) exec(ctx context.Context, fn func(reader *frameReader) error) error {
	address := net.JoinHostPort(c.host, strconv.Itoa(c.port))
	conn, err := net.DialTimeout("tcp", address, c.timeout)
	if err != nil {
		return fmt.Errorf("dial openwebnet: %w", err)
	}
	defer conn.Close()
	if err := applyDeadline(ctx, conn, c.timeout); err != nil {
		return err
	}

	reader := &frameReader{conn: conn}
	handshake := "*99*0##"
	c.emitTrace("tx", map[string]any{"transport": "tcp_command", "phase": "handshake", "frame": handshake})
	if _, err := conn.Write([]byte(handshake)); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}
	resp, err := reader.readFrame()
	if err != nil {
		return fmt.Errorf("read handshake: %w", err)
	}
	c.emitTrace("rx", map[string]any{"transport": "tcp_command", "phase": "handshake", "frame": resp, "mapped": false})

	switch strings.TrimSpace(resp) {
	case ownAckFrame:
	case "*98*2##":
		if c.password == "" {
			return ErrAuthenticationNeeded
		}
		if err := c.authenticateHMAC(reader); err != nil {
			return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
		}
	default:
		return fmt.Errorf("%w: %s", ErrHandshakeFailed, strings.TrimSpace(resp))
	}

	return fn(reader)
}

func (c *CommandClient) sendAndExpectSuccess(reader *frameReader, operation string, frame string, acceptedFrames ...string) error {
	c.emitTrace("tx", map[string]any{"transport": "tcp_command", "operation": operation, "frame": frame})
	if _, err := reader.conn.Write([]byte(frame)); err != nil {
		return err
	}

	resp, err := reader.readFrame()
	if err != nil {
		return err
	}

	frames := []string{resp}
	accepted := false
	terminal := ""
	switch {
	case resp == ownNackFrame:
		terminal = "nack"
	case resp == ownAckFrame:
		accepted = true
		terminal = "ack"
	case isAcceptedFrame(resp, acceptedFrames):
		accepted = true
		terminal = "accepted_frame"
	default:
		terminal = "unexpected"
	}

	if accepted {
		for i := 0; i < 8; i++ {
			next, timedOut, readErr := readFrameWithTimeout(reader, 40*time.Millisecond)
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					terminal = "accepted_eof"
					break
				}
				return readErr
			}
			if timedOut {
				terminal = "accepted_timeout"
				break
			}
			frames = append(frames, next)
			switch {
			case next == ownAckFrame:
				terminal = "ack"
			case next == ownNackFrame:
				accepted = false
				terminal = "nack"
				i = 8
			case isAcceptedFrame(next, acceptedFrames):
				terminal = "accepted_frame"
			default:
				accepted = false
				terminal = "unexpected"
				i = 8
			}
		}
	}

	finalFrame := frames[len(frames)-1]
	c.emitTrace("rx", map[string]any{
		"transport": "tcp_command",
		"operation": operation,
		"frame":     finalFrame,
		"frames":    frames,
		"accepted":  accepted,
		"terminal":  terminal,
	})
	if !accepted {
		return fmt.Errorf("%w: %s", ErrUnexpectedReply, strings.TrimSpace(finalFrame))
	}
	return nil
}

func (c *CommandClient) authenticateHMAC(reader *frameReader) error {
	c.emitTrace("tx", map[string]any{"transport": "tcp_command", "phase": "auth", "frame": ownAckFrame})
	if _, err := reader.conn.Write([]byte(ownAckFrame)); err != nil {
		return fmt.Errorf("send auth ack: %w", err)
	}

	challenge, err := reader.readFrame()
	if err != nil {
		return fmt.Errorf("read auth challenge: %w", err)
	}
	c.emitTrace("rx", map[string]any{"transport": "tcp_command", "phase": "auth", "frame": challenge})

	ra, err := challengeToHex(challenge)
	if err != nil {
		return err
	}
	rb := sha256Hex(fmt.Sprintf("time%d", time.Now().UnixMilli()))
	kab := sha256Hex(c.password)
	const (
		a = "736F70653E"
		b = "636F70653E"
	)
	hmac := sha256Hex(ra + rb + a + b + kab)
	payload := "*#" + hexToDigit(rb) + "*" + hexToDigit(hmac) + "##"
	c.emitTrace("tx", map[string]any{"transport": "tcp_command", "phase": "auth", "frame": payload})
	if _, err := reader.conn.Write([]byte(payload)); err != nil {
		return fmt.Errorf("send auth payload: %w", err)
	}

	serverHMAC, err := reader.readFrame()
	if err != nil {
		return fmt.Errorf("read auth server hmac: %w", err)
	}
	c.emitTrace("rx", map[string]any{"transport": "tcp_command", "phase": "auth", "frame": serverHMAC})
	if !strings.HasPrefix(serverHMAC, "*#") || !strings.HasSuffix(serverHMAC, "##") {
		return fmt.Errorf("unexpected auth server payload: %s", serverHMAC)
	}

	c.emitTrace("tx", map[string]any{"transport": "tcp_command", "phase": "auth", "frame": ownAckFrame})
	if _, err := reader.conn.Write([]byte(ownAckFrame)); err != nil {
		return fmt.Errorf("send auth final ack: %w", err)
	}
	finalAck, err := reader.readFrame()
	if err != nil {
		return fmt.Errorf("read auth final ack: %w", err)
	}
	c.emitTrace("rx", map[string]any{"transport": "tcp_command", "phase": "auth", "frame": finalAck})
	if strings.TrimSpace(finalAck) != ownAckFrame {
		return fmt.Errorf("unexpected auth final ack: %s", finalAck)
	}
	return nil
}

func isAcceptedFrame(frame string, acceptedFrames []string) bool {
	for _, candidate := range acceptedFrames {
		if frame == candidate {
			return true
		}
	}
	return false
}

func readFrameWithTimeout(reader *frameReader, timeout time.Duration) (string, bool, error) {
	if err := reader.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", false, err
	}
	frame, err := reader.readFrame()
	clearErr := reader.conn.SetReadDeadline(time.Time{})
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			if clearErr != nil {
				return "", false, clearErr
			}
			return "", true, nil
		}
		if clearErr != nil {
			return "", false, clearErr
		}
		return "", false, err
	}
	if clearErr != nil {
		return "", false, clearErr
	}
	return frame, false, nil
}

func challengeToHex(frame string) (string, error) {
	if !strings.HasPrefix(frame, "*#") || !strings.HasSuffix(frame, "##") {
		return "", fmt.Errorf("invalid auth challenge: %s", frame)
	}
	digits := strings.TrimSuffix(strings.TrimPrefix(frame, "*#"), "##")
	if digits == "" || len(digits)%4 != 0 {
		return "", fmt.Errorf("invalid auth challenge digits: %s", frame)
	}
	return digitToHex(digits)
}

func digitToHex(digits string) (string, error) {
	var builder strings.Builder
	for i := 0; i < len(digits); i += 4 {
		pairA, err := strconv.Atoi(digits[i : i+2])
		if err != nil {
			return "", fmt.Errorf("invalid auth challenge pair: %w", err)
		}
		pairB, err := strconv.Atoi(digits[i+2 : i+4])
		if err != nil {
			return "", fmt.Errorf("invalid auth challenge pair: %w", err)
		}
		builder.WriteString(fmt.Sprintf("%02x", pairA))
		builder.WriteString(fmt.Sprintf("%02x", pairB))
	}
	return builder.String(), nil
}

func hexToDigit(hexString string) string {
	var builder strings.Builder
	for _, c := range hexString {
		v, err := strconv.ParseInt(string(c), 16, 64)
		if err != nil {
			continue
		}
		if v < 10 {
			builder.WriteByte('0')
		}
		builder.WriteString(strconv.FormatInt(v, 10))
	}
	return builder.String()
}

func sha256Hex(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

func (r *frameReader) readFrame() (string, error) {
	tmp := make([]byte, 128)
	for {
		if idx := frameRegexp.FindStringIndex(r.pending); idx != nil {
			frame := r.pending[idx[0]:idx[1]]
			r.pending = r.pending[idx[1]:]
			return frame, nil
		}

		n, err := r.conn.Read(tmp)
		if err != nil {
			return "", err
		}
		r.pending += string(tmp[:n])
		if len(r.pending) > 4096 {
			r.pending = r.pending[len(r.pending)-4096:]
		}
	}
}

func applyDeadline(ctx context.Context, conn net.Conn, fallback time.Duration) error {
	deadline := time.Now().Add(fallback)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set conn deadline: %w", err)
	}
	return nil
}

func (c *CommandClient) emitTrace(direction string, payload map[string]any) {
	if c.traceSink == nil {
		return
	}
	c.traceSink(direction, payload)
}

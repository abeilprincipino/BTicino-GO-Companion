package openwebnet

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	ErrAuthenticationNeeded = errors.New("openwebnet authentication required")
	ErrCommandRejected      = errors.New("openwebnet command rejected")
	ErrUnexpectedReply      = errors.New("openwebnet unexpected reply")
	ErrVoicemailUnavailable = errors.New("openwebnet voicemail unavailable")
	frameRegexp             = regexp.MustCompile(`\*#?.*?##`)
)

const (
	commandHost = "127.0.0.1"
	commandPort = 20000
)

// Control implements the V3 entrypoint, audio, and voicemail control interfaces.
type Control struct {
	host        string
	port        int
	password    string
	entrypoints map[core.EntrypointID]string
	timeout     time.Duration
	trace       *Trace
}
type VoicemailStatus struct{ Enabled bool }

// DiagnosticSnapshot contains device metadata reported by OpenWebNet.
type DiagnosticSnapshot struct {
	IP           string `json:"ip,omitempty"`
	Netmask      string `json:"netmask,omitempty"`
	MAC          string `json:"mac,omitempty"`
	Firmware     string `json:"firmware,omitempty"`
	Hardware     string `json:"hardware,omitempty"`
	Kernel       string `json:"kernel,omitempty"`
	Distribution string `json:"distribution,omitempty"`
}

func NewControl(entrypoints []config.Entrypoint, trace *Trace) *Control {
	addresses := make(map[core.EntrypointID]string, len(entrypoints))
	for _, entrypoint := range entrypoints {
		addresses[core.EntrypointID(entrypoint.ID)] = entrypoint.DevAddr
	}

	return &Control{host: commandHost, port: commandPort, entrypoints: addresses, timeout: 3 * time.Second, trace: trace}
}

func (c *Control) Unlock(ctx context.Context, id core.EntrypointID) error {
	address := c.entrypoints[id]
	if address == "" {
		return fmt.Errorf("unknown entrypoint %q", id)
	}

	return c.exec(ctx, func(reader *frameReader) error {
		if err := c.send(reader, BuildUnlockOpen(address), FrameACK); err != nil {
			return fmt.Errorf("unlock open: %w", err)
		}

		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}

		if err := c.send(reader, BuildUnlockClose(address), FrameACK); err != nil {
			return fmt.Errorf("unlock close: %w", err)
		}

		return nil
	})
}

func (c *Control) Stream(context.Context, core.EntrypointID) error {
	return errors.New("stream control is unavailable")
}

func (c *Control) Snapshot(context.Context, core.EntrypointID) ([]byte, error) {
	return nil, errors.New("snapshot control is unavailable")
}

func (c *Control) Mute(ctx context.Context) error {
	return c.command(ctx, FrameAudioMuteCmd, FrameACK, FrameAudioMuted)
}

func (c *Control) Unmute(ctx context.Context) error {
	return c.command(ctx, FrameAudioUnmuteCmd, FrameACK, FrameAudioUnmuted)
}

func (c *Control) Enable(ctx context.Context) error {
	if _, err := c.VoicemailStatus(ctx); err != nil {
		return fmt.Errorf("check voicemail availability: %w", err)
	}

	return c.command(ctx, FrameVoicemailEnableCmd, FrameACK)
}

func (c *Control) Disable(ctx context.Context) error {
	if _, err := c.VoicemailStatus(ctx); err != nil {
		return fmt.Errorf("check voicemail availability: %w", err)
	}

	return c.command(ctx, FrameVoicemailDisableCmd, FrameACK)
}

func (c *Control) InitialEvents(ctx context.Context) ([]core.Event, error) {
	events := make([]core.Event, 0, 2)

	var errs []error

	if muted, err := c.AudioMutedStatus(ctx); err == nil {
		if muted {
			events = append(events, core.AudioMuted{})
		} else {
			events = append(events, core.AudioUnmuted{})
		}
	} else {
		errs = append(errs, fmt.Errorf("read audio status: %w", err))
	}

	if voicemail, err := c.VoicemailStatus(ctx); err == nil {
		if voicemail.Enabled {
			events = append(events, core.VoicemailEnabled{})
		} else {
			events = append(events, core.VoicemailDisabled{})
		}
	} else {
		errs = append(errs, fmt.Errorf("read voicemail status: %w", err))
	}

	return events, errors.Join(errs...)
}

func (c *Control) AudioMutedStatus(ctx context.Context) (bool, error) {
	frame, err := c.status(ctx, FrameAudioStatusCmd, FrameAudioMuted, FrameAudioUnmuted)
	if err != nil {
		return false, err
	}

	return frame == FrameAudioMuted, nil
}

func (c *Control) VoicemailStatus(ctx context.Context) (VoicemailStatus, error) {
	frame, err := c.status(ctx, FrameVoicemailStatusCmd)
	if err != nil {
		if errors.Is(err, ErrCommandRejected) {
			return VoicemailStatus{}, fmt.Errorf("%w: %w", ErrVoicemailUnavailable, err)
		}

		return VoicemailStatus{}, err
	}

	enabled, _, ok := ParseVoicemailStatus(frame)
	if !ok {
		return VoicemailStatus{}, fmt.Errorf("%w: %s", ErrUnexpectedReply, frame)
	}

	return VoicemailStatus{Enabled: enabled}, nil
}

func (c *Control) DiagnosticSnapshot(ctx context.Context) (DiagnosticSnapshot, error) {
	var snapshot DiagnosticSnapshot

	err := c.exec(ctx, func(reader *frameReader) error {
		for _, query := range []struct {
			command string
			parse   func(string) (string, bool)
			set     func(string)
		}{
			{FrameDiagIPCmd, ParseDiagnosticIP, func(value string) { snapshot.IP = value }},
			{FrameDiagNetmaskCmd, ParseDiagnosticNetmask, func(value string) { snapshot.Netmask = value }},
			{FrameDiagMACCmd, ParseDiagnosticMAC, func(value string) { snapshot.MAC = value }},
			{FrameDiagFirmwareCmd, ParseDiagnosticFirmware, func(value string) { snapshot.Firmware = value }},
			{FrameDiagHardwareCmd, ParseDiagnosticHardware, func(value string) { snapshot.Hardware = value }},
			{FrameDiagKernelCmd, ParseDiagnosticKernel, func(value string) { snapshot.Kernel = value }},
			{FrameDiagDistributionCmd, ParseDiagnosticDistribution, func(value string) { snapshot.Distribution = value }},
		} {
			frame, err := c.sendStatus(reader, query.command)
			if err != nil {
				return fmt.Errorf("read diagnostic %s: %w", query.command, err)
			}

			value, ok := query.parse(frame)
			if !ok {
				return fmt.Errorf("read diagnostic %s: %w", query.command, ErrUnexpectedReply)
			}

			query.set(value)
		}

		return nil
	})

	return snapshot, err
}

func (c *Control) command(ctx context.Context, frame string, accepted ...string) error {
	return c.exec(ctx, func(reader *frameReader) error { return c.send(reader, frame, accepted...) })
}

func (c *Control) status(ctx context.Context, command string, accepted ...string) (string, error) {
	var result string

	err := c.exec(ctx, func(reader *frameReader) error {
		frame, err := c.sendStatus(reader, command, accepted...)
		if err != nil {
			return err
		}

		result = frame

		return nil
	})

	return result, err
}

func (c *Control) exec(ctx context.Context, fn func(*frameReader) error) error {
	dialer := net.Dialer{Timeout: c.timeout}

	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(c.host, strconv.Itoa(c.port)))
	if err != nil {
		return fmt.Errorf("dial openwebnet: %w", err)
	}
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(c.timeout)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}

	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set openwebnet deadline: %w", err)
	}

	reader := &frameReader{conn: conn, deadline: deadline}
	if _, err := conn.Write([]byte(FrameSessionStartCmd)); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}

	c.trace.RecordTCP("TX", FrameSessionStartCmd)

	response, err := reader.read()
	if err != nil {
		return fmt.Errorf("read handshake: %w", err)
	}

	c.trace.RecordTCP("RX", response)

	switch response {
	case FrameACK:
	case FrameAuthRequired:
		if c.password == "" {
			return ErrAuthenticationNeeded
		}

		if err := c.authenticate(reader); err != nil {
			return fmt.Errorf("authenticate: %w", err)
		}
	default:
		return fmt.Errorf("%w: %s", ErrUnexpectedReply, response)
	}

	return fn(reader)
}

func (c *Control) send(reader *frameReader, frame string, accepted ...string) error {
	response, err := c.sendFrame(reader, frame)
	if err != nil {
		return err
	}

	if response == FrameNACK || (response != FrameACK && !acceptedFrame(response, accepted)) {
		return fmt.Errorf("%w: %s", ErrUnexpectedReply, response)
	}

	if response != FrameACK {
		return nil
	}

	for range 8 {
		next, timedOut, err := readFrameWithTimeout(reader, 40*time.Millisecond)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}

		if timedOut {
			return nil
		}

		if next == FrameNACK || (!acceptedFrame(next, accepted) && next != FrameACK) {
			return fmt.Errorf("%w: %s", ErrUnexpectedReply, next)
		}
	}

	return nil
}

func (c *Control) sendStatus(reader *frameReader, command string, accepted ...string) (string, error) {
	frame, err := c.sendFrame(reader, command)
	if err != nil {
		return "", err
	}

	if frame == FrameNACK {
		return "", fmt.Errorf("%w: %s", ErrCommandRejected, frame)
	}

	if len(accepted) == 0 && frame != FrameACK {
		return frame, nil
	}

	if acceptedFrame(frame, accepted) {
		return frame, nil
	}

	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		next, timedOut, err := readFrameWithTimeout(reader, 120*time.Millisecond)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return "", err
		}

		if timedOut {
			continue
		}

		if next == FrameNACK {
			return "", fmt.Errorf("%w: %s", ErrCommandRejected, next)
		}

		if len(accepted) == 0 && next != FrameACK {
			return next, nil
		}

		if acceptedFrame(next, accepted) {
			return next, nil
		}
	}

	return "", ErrUnexpectedReply
}

func acceptedFrame(frame string, accepted []string) bool {
	return slices.Contains(accepted, frame)
}

func (c *Control) sendFrame(reader *frameReader, frame string) (string, error) {
	if _, err := reader.conn.Write([]byte(frame)); err != nil {
		return "", err
	}

	c.trace.RecordTCP("TX", frame)

	response, err := reader.read()
	if err != nil {
		return "", err
	}

	c.trace.RecordTCP("RX", response)

	return response, nil
}

func (c *Control) authenticate(reader *frameReader) error {
	if _, err := reader.conn.Write([]byte(FrameACK)); err != nil {
		return err
	}

	challenge, err := reader.read()
	if err != nil {
		return err
	}

	ra, err := challengeToHex(challenge)
	if err != nil {
		return err
	}

	rb := sha256Hex(fmt.Sprintf("time%d", time.Now().UnixMilli()))

	hmac := sha256Hex(ra + rb + "736F70653E" + "636F70653E" + sha256Hex(c.password))
	if _, err := reader.conn.Write([]byte("*#" + hexToDigit(rb) + "*" + hexToDigit(hmac) + "##")); err != nil {
		return err
	}

	if _, err := reader.read(); err != nil {
		return err
	}

	if _, err := reader.conn.Write([]byte(FrameACK)); err != nil {
		return err
	}

	response, err := reader.read()
	if err != nil {
		return err
	}

	if response != FrameACK {
		return fmt.Errorf("%w: %s", ErrUnexpectedReply, response)
	}

	return nil
}

type frameReader struct {
	conn     net.Conn
	pending  string
	deadline time.Time
}

func (r *frameReader) read() (string, error) {
	buf := make([]byte, 128)

	for {
		if match := frameRegexp.FindStringIndex(r.pending); match != nil {
			frame := r.pending[match[0]:match[1]]
			r.pending = r.pending[match[1]:]

			return frame, nil
		}

		n, err := r.conn.Read(buf)
		if err != nil {
			return "", err
		}

		r.pending += string(buf[:n])
		if len(r.pending) > 4096 {
			r.pending = r.pending[len(r.pending)-4096:]
		}
	}
}

func readFrameWithTimeout(reader *frameReader, timeout time.Duration) (string, bool, error) {
	if err := reader.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", false, err
	}

	frame, err := reader.read()
	if restoreErr := reader.conn.SetReadDeadline(reader.deadline); restoreErr != nil {
		return "", false, restoreErr
	}

	if err == nil {
		return frame, false, nil
	}

	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "", true, nil
	}

	return "", false, err
}

func challengeToHex(frame string) (string, error) {
	if !strings.HasPrefix(frame, "*#") || !strings.HasSuffix(frame, "##") {
		return "", errors.New("invalid auth challenge")
	}

	digits := strings.TrimSuffix(strings.TrimPrefix(frame, "*#"), "##")
	if digits == "" || len(digits)%4 != 0 {
		return "", errors.New("invalid auth challenge digits")
	}

	var builder strings.Builder

	for i := 0; i < len(digits); i += 4 {
		a, err := strconv.Atoi(digits[i : i+2])
		if err != nil {
			return "", err
		}

		b, err := strconv.Atoi(digits[i+2 : i+4])
		if err != nil {
			return "", err
		}

		fmt.Fprintf(&builder, "%02x%02x", a, b)
	}

	return builder.String(), nil
}

func hexToDigit(value string) string {
	var builder strings.Builder

	for _, char := range value {
		parsed, err := strconv.ParseInt(string(char), 16, 64)
		if err != nil {
			continue
		}

		if parsed < 10 {
			builder.WriteByte('0')
		}

		builder.WriteString(strconv.FormatInt(parsed, 10))
	}

	return builder.String()
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

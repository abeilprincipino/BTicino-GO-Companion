package openwebnet

import (
	"bticino-go-companion/internal/media"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

const (
	AVHost = "127.0.0.1"
	AVPort = 30007
)

var (
	ErrAVCommandRejected = errors.New("openwebnet av command rejected")
	ErrAVFlowTimeout     = errors.New("openwebnet av rtp flow did not start")
)

// AVClient starts the intercom clear-RTP streams.
type AVClient struct {
	logger  *slog.Logger
	address string
	// retryWindow is how long a rejected request is retried: the intercom
	// refuses video until its own stream is up, which it decides, so the
	// request is repeated every retryDelay until then rather than a fixed
	// number of times.
	retryWindow  time.Duration
	dialTimeout  time.Duration
	replyTimeout time.Duration
	retryDelay   time.Duration
	flowTimeout  time.Duration
	flowPoll     time.Duration
	flowWindow   time.Duration
	// readiness, when set, holds a ringing preview's first video request until
	// the intercom has announced the ring's video parameters, for at most
	// readinessWait.
	readiness     *AVReadiness
	readinessWait time.Duration
}

func NewAVClient(logger *slog.Logger) *AVClient {
	if logger == nil {
		logger = slog.Default()
	}

	return &AVClient{
		logger:        logger.With("component", "media.activation"),
		address:       net.JoinHostPort(AVHost, "30007"),
		retryWindow:   6 * time.Second,
		dialTimeout:   2 * time.Second,
		replyTimeout:  2 * time.Second,
		retryDelay:    100 * time.Millisecond,
		flowTimeout:   1200 * time.Millisecond,
		flowPoll:      100 * time.Millisecond,
		flowWindow:    2 * time.Second,
		readinessWait: time.Second,
	}
}

// SetReadiness makes StartVideo wait for the ring's video parameters.
func (c *AVClient) SetReadiness(readiness *AVReadiness) {
	c.readiness = readiness
}

// Start requests video then audio. Each request must receive an ACK or be
// corroborated by observed RTP, and both streams must subsequently flow.
func (c *AVClient) Start(ctx context.Context, highRes bool, ports media.AVPorts, video, audio media.FlowProbe) error {
	if video == nil || audio == nil {
		return errors.New("openwebnet av flow probes are required")
	}

	if err := c.StartVideo(ctx, highRes, ports.Video, video); err != nil {
		return err
	}

	return c.StartAudio(ctx, ports.Audio, audio)
}

// StartVideo requests the video stream alone. A ringing preview starts with it:
// the intercom has no audio to give before the call is answered. See
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
func (c *AVClient) StartVideo(ctx context.Context, highRes bool, port int, probe media.FlowProbe) error {
	if probe == nil {
		return errors.New("openwebnet av video flow probe is required")
	}

	resolution := "low"
	if highRes {
		resolution = "high"
	}

	c.logger.InfoContext(ctx, "av activation starting", "video_resolution", resolution)

	// Only a ringing preview starts with video alone, and only a ring makes
	// the intercom announce its video parameters.
	if c.readiness != nil && !probe.RecentlyFlowing(c.flowWindow) {
		waitStarted := time.Now()
		ready := c.readiness.WaitVideoParameters(ctx, c.readinessWait)
		c.logger.DebugContext(ctx, "av video parameters wait", "ready", ready, "waited", time.Since(waitStarted))
	}

	return c.startStream(ctx, "video", BuildAVAddStreamVideo("127.0.0.1", port, highRes), probe)
}

// StartAudio requests the audio stream alone, on a port the session bound at
// start. It is how a ringing preview gains audio once the call is answered,
// without restarting anything.
func (c *AVClient) StartAudio(ctx context.Context, port int, probe media.FlowProbe) error {
	if probe == nil {
		return errors.New("openwebnet av audio flow probe is required")
	}

	return c.startStream(ctx, "audio", BuildAVAddStreamAudio("127.0.0.1", port), probe)
}

func (c *AVClient) startStream(ctx context.Context, kind, frame string, probe media.FlowProbe) error {
	if probe.RecentlyFlowing(c.flowWindow) {
		c.logger.DebugContext(ctx, "av stream already flowing", "stream", kind)
		return nil
	}

	var lastErr error

	deadline := time.Now().Add(c.retryWindow)
	attempt := 0

	for attempt == 0 || time.Now().Before(deadline) {
		attempt++

		if attempt > 1 {
			c.logger.DebugContext(ctx, "av stream retrying", "stream", kind, "attempt", attempt, "retry_delay", c.retryDelay)

			if err := wait(ctx, c.retryDelay); err != nil {
				return fmt.Errorf("wait to retry %s stream: %w", kind, err)
			}
		}

		c.logger.DebugContext(ctx, "av stream request", "stream", kind, "attempt", attempt)

		requested := time.Now()
		ack, err := c.exchange(ctx, frame)
		c.logger.DebugContext(ctx, "av stream reply", "stream", kind, "attempt", attempt, "ack", ack, "error", err, "elapsed", time.Since(requested))

		if err != nil {
			lastErr = err
		} else if !ack {
			lastErr = ErrAVCommandRejected
		}

		if ack || probe.RecentlyFlowing(c.flowWindow) {
			if c.waitForFlow(ctx, probe) {
				c.logger.InfoContext(ctx, "av stream flowing", "stream", kind, "attempt", attempt)
				return nil
			}

			lastErr = ErrAVFlowTimeout
		}

		c.logger.DebugContext(ctx, "av stream attempt failed", "stream", kind, "attempt", attempt, "error", lastErr)
	}

	c.logger.WarnContext(ctx, "av stream start failed", "stream", kind, "attempts", attempt, "error", lastErr)

	return fmt.Errorf("start %s stream after %d attempts: %w", kind, attempt, lastErr)
}

func (c *AVClient) exchange(ctx context.Context, frame string) (bool, error) {
	dialer := net.Dialer{Timeout: c.dialTimeout}

	conn, err := dialer.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return false, fmt.Errorf("dial av endpoint: %w", err)
	}
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(c.replyTimeout)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}

	if err := conn.SetDeadline(deadline); err != nil {
		return false, fmt.Errorf("set av deadline: %w", err)
	}

	if _, err := conn.Write([]byte(frame)); err != nil {
		return false, fmt.Errorf("write av command: %w", err)
	}

	buf := make([]byte, 256)

	n, err := conn.Read(buf)
	if err != nil {
		return false, fmt.Errorf("read av reply: %w", err)
	}

	return allACKs(string(buf[:n])), nil
}

func (c *AVClient) waitForFlow(ctx context.Context, probe media.FlowProbe) bool {
	if probe.RecentlyFlowing(c.flowWindow) {
		return true
	}

	deadline, cancel := context.WithTimeout(ctx, c.flowTimeout)
	defer cancel()

	ticker := time.NewTicker(c.flowPoll)
	defer ticker.Stop()

	for {
		select {
		case <-deadline.Done():
			return probe.RecentlyFlowing(c.flowWindow)
		case <-ticker.C:
			if probe.RecentlyFlowing(c.flowWindow) {
				return true
			}
		}
	}
}

func allACKs(reply string) bool {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return false
	}

	for reply != "" {
		if !strings.HasPrefix(reply, FrameACK) {
			return false
		}

		reply = strings.TrimPrefix(reply, FrameACK)
	}

	return true
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

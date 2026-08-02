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
	logger       *slog.Logger
	address      string
	attempts     int
	dialTimeout  time.Duration
	replyTimeout time.Duration
	retryDelay   time.Duration
	flowTimeout  time.Duration
	flowPoll     time.Duration
	flowWindow   time.Duration
}

func NewAVClient(logger *slog.Logger) *AVClient {
	if logger == nil {
		logger = slog.Default()
	}

	return &AVClient{
		logger:       logger.With("component", "media.activation"),
		address:      net.JoinHostPort(AVHost, "30007"),
		attempts:     3,
		dialTimeout:  2 * time.Second,
		replyTimeout: 2 * time.Second,
		retryDelay:   300 * time.Millisecond,
		flowTimeout:  1200 * time.Millisecond,
		flowPoll:     100 * time.Millisecond,
		flowWindow:   2 * time.Second,
	}
}

// Start requests video then audio. Each request must receive an ACK or be
// corroborated by observed RTP, and both streams must subsequently flow.
func (c *AVClient) Start(ctx context.Context, highRes bool, ports media.AVPorts, video, audio media.FlowProbe) error {
	if video == nil || audio == nil {
		return errors.New("openwebnet av flow probes are required")
	}

	resolution := "low"
	if highRes {
		resolution = "high"
	}

	c.logger.InfoContext(ctx, "av activation starting", "video_resolution", resolution)

	if err := c.startStream(ctx, "video", BuildAVAddStreamVideo("127.0.0.1", ports.Video, highRes), video); err != nil {
		return err
	}

	if err := c.startStream(ctx, "audio", BuildAVAddStreamAudio("127.0.0.1", ports.Audio), audio); err != nil {
		return err
	}

	return nil
}

func (c *AVClient) startStream(ctx context.Context, kind, frame string, probe media.FlowProbe) error {
	if probe.RecentlyFlowing(c.flowWindow) {
		c.logger.DebugContext(ctx, "av stream already flowing", "stream", kind)
		return nil
	}

	var lastErr error

	for attempt := 1; attempt <= c.attempts; attempt++ {
		if attempt > 1 {
			c.logger.DebugContext(ctx, "av stream retrying", "stream", kind, "attempt", attempt, "retry_delay", c.retryDelay)

			if err := wait(ctx, c.retryDelay); err != nil {
				return fmt.Errorf("wait to retry %s stream: %w", kind, err)
			}
		}

		c.logger.DebugContext(ctx, "av stream request", "stream", kind, "attempt", attempt)

		ack, err := c.exchange(ctx, frame)
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

	c.logger.WarnContext(ctx, "av stream start failed", "stream", kind, "attempts", c.attempts, "error", lastErr)

	return fmt.Errorf("start %s stream after %d attempts: %w", kind, c.attempts, lastErr)
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

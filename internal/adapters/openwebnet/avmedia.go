package openwebnet

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"bticino-go-companion/internal/config"
	openwebnetproto "bticino-go-companion/internal/protocol/openwebnet"
)

// ErrAVCommandRejected reports that the bt_ipcamera AV endpoint refused or did
// not acknowledge an add-stream command.
var ErrAVCommandRejected = errors.New("av endpoint rejected command")

// AVMediaClient talks to the intercom's AV server (bt_ipcamera, default
// 127.0.0.1:30007) over raw TCP — no openserver session handshake. It mirrors
// the c300x-controller's bt-av-media semantics: write an add-stream frame,
// expect *#*1## (possibly doubled), retry on *#*0##.
type AVMediaClient struct {
	addr         string
	streamIP     string
	highRes      bool
	dialTimeout  time.Duration
	replyTimeout time.Duration
	retryDelay   time.Duration
	maxAttempts  int
	audioDelay   time.Duration
	logger       *log.Logger

	mu   sync.Mutex
	conn net.Conn
}

func NewAVMediaClient(cfg config.Config, logger *log.Logger) *AVMediaClient {
	return &AVMediaClient{
		addr:         net.JoinHostPort(strings.TrimSpace(cfg.MediaAVEndpointHost), strconv.Itoa(cfg.MediaAVEndpointPort)),
		streamIP:     "127.0.0.1",
		highRes:      cfg.MediaAVHighResVideo,
		dialTimeout:  5 * time.Second,
		replyTimeout: 5 * time.Second,
		retryDelay:   time.Second,
		maxAttempts:  3,
		audioDelay:   300 * time.Millisecond,
		logger:       logger,
	}
}

// StreamStart directs the intercom's RTP streams to the companion's ingest
// ports: video first, then audio after a short delay (matches the controller).
func (c *AVMediaClient) StreamStart(ctx context.Context, audioPort, videoPort int) error {
	if audioPort <= 0 || videoPort <= 0 {
		return errors.New("invalid av stream ports")
	}
	video := openwebnetproto.BuildAVAddStreamVideo(c.streamIP, videoPort, c.highRes)
	if err := c.sendCommand(ctx, "add-video-stream", video); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(c.audioDelay):
	}
	audio := openwebnetproto.BuildAVAddStreamAudio(c.streamIP, audioPort)
	return c.sendCommand(ctx, "add-audio-stream", audio)
}

func (c *AVMediaClient) sendCommand(ctx context.Context, label, frame string) error {
	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.retryDelay):
			}
		}
		reply, err := c.exchange(frame)
		if err != nil {
			lastErr = err
			c.logf("av %s attempt %d/%d transport error: %v", label, attempt, c.maxAttempts, err)
			continue
		}
		c.logf("av %s attempt %d/%d frame=%q reply=%q", label, attempt, c.maxAttempts, frame, reply)
		if isAllACKs(reply) {
			return nil
		}
		if reply == openwebnetproto.FrameNACK {
			lastErr = fmt.Errorf("%w: NAK", ErrAVCommandRejected)
			continue
		}
		c.closeConn()
		lastErr = fmt.Errorf("%w: unexpected reply %q", ErrAVCommandRejected, reply)
	}
	return fmt.Errorf("av %s failed after %d attempts: %w", label, c.maxAttempts, lastErr)
}

// exchange writes one frame and reads one reply, reusing the connection when
// still healthy and re-dialing otherwise.
func (c *AVMediaClient) exchange(frame string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		conn, err := net.DialTimeout("tcp", c.addr, c.dialTimeout)
		if err != nil {
			return "", fmt.Errorf("dial %s: %w", c.addr, err)
		}
		c.conn = conn
	}
	if err := c.conn.SetDeadline(time.Now().Add(c.replyTimeout)); err != nil {
		c.closeConnLocked()
		return "", fmt.Errorf("set deadline: %w", err)
	}
	if _, err := c.conn.Write([]byte(frame)); err != nil {
		c.closeConnLocked()
		return "", fmt.Errorf("write frame: %w", err)
	}
	buf := make([]byte, 256)
	n, err := c.conn.Read(buf)
	if err != nil {
		c.closeConnLocked()
		return "", fmt.Errorf("read reply: %w", err)
	}
	return strings.TrimSpace(string(buf[:n])), nil
}

func (c *AVMediaClient) closeConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeConnLocked()
}

func (c *AVMediaClient) closeConnLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

func (c *AVMediaClient) logf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Printf(format, args...)
	}
}

// isAllACKs accepts a single ACK and the doubled *#*1##*#*1## reply that
// bt_ipcamera emits on concurrent setups.
func isAllACKs(reply string) bool {
	if reply == "" {
		return false
	}
	rest := reply
	for rest != "" {
		if !strings.HasPrefix(rest, openwebnetproto.FrameACK) {
			return false
		}
		rest = strings.TrimPrefix(rest, openwebnetproto.FrameACK)
	}
	return true
}

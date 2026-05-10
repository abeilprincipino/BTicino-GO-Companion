package openwebnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/protocol/openwebnet"
)

var (
	ErrHandshakeFailed = errors.New("openwebnet handshake failed")
	ErrUnexpectedReply = errors.New("openwebnet unexpected reply")
)

var frameRegexp = regexp.MustCompile(`\*#?.*?##`)

type CommandClient struct {
	host        string
	port        int
	timeout     time.Duration
	unlockDelay time.Duration
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
		unlockDelay: 2 * time.Second,
	}
}

func (c *CommandClient) Unlock(ctx context.Context, devAddr string) error {
	if strings.TrimSpace(devAddr) == "" {
		return errors.New("empty devaddr")
	}
	return c.exec(ctx, func(reader *frameReader) error {
		if err := sendAndExpect(reader, openwebnetproto.BuildUnlockOpen(devAddr)); err != nil {
			return fmt.Errorf("unlock open: %w", err)
		}
		timer := time.NewTimer(c.unlockDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if err := sendAndExpect(reader, openwebnetproto.BuildUnlockClose(devAddr)); err != nil {
			return fmt.Errorf("unlock close: %w", err)
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
	if _, err := conn.Write([]byte("*99*0##")); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}
	resp, err := reader.readFrame()
	if err != nil {
		return fmt.Errorf("read handshake: %w", err)
	}
	if !openwebnetproto.IsACK(resp) {
		return fmt.Errorf("%w: %s", ErrHandshakeFailed, strings.TrimSpace(resp))
	}
	return fn(reader)
}

func sendAndExpect(reader *frameReader, frame string) error {
	if _, err := reader.conn.Write([]byte(frame)); err != nil {
		return err
	}
	resp, err := reader.readFrame()
	if err != nil {
		return err
	}
	if !openwebnetproto.IsACK(resp) {
		return fmt.Errorf("%w: %s", ErrUnexpectedReply, strings.TrimSpace(resp))
	}
	return nil
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

package openwebnet

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strconv"
	"testing"
	"time"

	"bticino-go-companion/internal/config"
)

type testFrameReader struct {
	conn    net.Conn
	pending string
}

func (r *testFrameReader) readFrame(t *testing.T) string {
	t.Helper()
	for {
		if idx := frameRegexp.FindStringIndex(r.pending); idx != nil {
			frame := r.pending[idx[0]:idx[1]]
			r.pending = r.pending[idx[1]:]
			return frame
		}
		buf := make([]byte, 256)
		n, err := r.conn.Read(buf)
		if err != nil {
			t.Fatalf("server read failed: %v", err)
		}
		r.pending += string(buf[:n])
	}
}

func TestCommandClientUnlock(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		reader := &testFrameReader{conn: conn}

		if got := reader.readFrame(t); got != "*99*0##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))

		if got := reader.readFrame(t); got != "*8*19*21##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))

		if got := reader.readFrame(t); got != "*8*20*21##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))
		done <- nil
	}()

	host, portRaw, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portRaw)
	cfg := config.Default()
	cfg.OpenWebNetCommandHost = host
	cfg.OpenWebNetCommandPort = port
	cfg.OpenWebNetCommandSec = 1

	c := NewCommandClient(cfg)
	c.unlockDelay = time.Millisecond
	if err := c.Unlock(context.Background(), "21"); err != nil {
		t.Fatalf("unlock failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server flow failed: %v", err)
	}
}

func TestCommandClientUnlockWithAuthChallenge(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		reader := &testFrameReader{conn: conn}

		if got := reader.readFrame(t); got != "*99*0##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*98*2##"))

		if got := reader.readFrame(t); got != "*#*1##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#00010203##"))

		payload := reader.readFrame(t)
		if ok, _ := regexp.MatchString(`^\*#[0-9]+\*[0-9]+##$`, payload); !ok {
			done <- errUnexpected(payload)
			return
		}
		_, _ = conn.Write([]byte("*#1234##"))

		if got := reader.readFrame(t); got != "*#*1##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))

		if got := reader.readFrame(t); got != "*8*19*21##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))

		if got := reader.readFrame(t); got != "*8*20*21##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))
		done <- nil
	}()

	host, portRaw, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portRaw)
	cfg := config.Default()
	cfg.OpenWebNetCommandHost = host
	cfg.OpenWebNetCommandPort = port
	cfg.OpenWebNetCommandSec = 1
	cfg.OpenWebNetCommandPassword = "12345"

	c := NewCommandClient(cfg)
	c.unlockDelay = time.Millisecond
	if err := c.Unlock(context.Background(), "21"); err != nil {
		t.Fatalf("unlock with auth challenge failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server flow failed: %v", err)
	}
}

func TestCommandClientAuthChallengeWithoutPassword(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		reader := &testFrameReader{conn: conn}
		if got := reader.readFrame(t); got != "*99*0##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*98*2##"))
		done <- nil
	}()

	host, portRaw, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portRaw)
	cfg := config.Default()
	cfg.OpenWebNetCommandHost = host
	cfg.OpenWebNetCommandPort = port
	cfg.OpenWebNetCommandSec = 1
	cfg.OpenWebNetCommandPassword = ""

	c := NewCommandClient(cfg)
	err = c.Unlock(context.Background(), "21")
	if !errors.Is(err, ErrAuthenticationNeeded) {
		t.Fatalf("expected ErrAuthenticationNeeded, got: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server flow failed: %v", err)
	}
}

func TestCommandClientStreamStart(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		reader := &testFrameReader{conn: conn}

		if got := reader.readFrame(t); got != "*99*0##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))

		if got := reader.readFrame(t); got != "*7*300#127#0#0#1#5007#0*##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##*#*1##"))

		if got := reader.readFrame(t); got != "*7*300#127#0#0#1#5000#2*##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))
		done <- nil
	}()

	host, portRaw, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portRaw)
	cfg := config.Default()
	cfg.OpenWebNetCommandHost = host
	cfg.OpenWebNetCommandPort = port
	cfg.OpenWebNetCommandSec = 1

	c := NewCommandClient(cfg)
	if err := c.StreamStart(context.Background(), 5000, 5007); err != nil {
		t.Fatalf("stream start failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server flow failed: %v", err)
	}
}

type errUnexpected string

func (e errUnexpected) Error() string { return "unexpected frame: " + string(e) }

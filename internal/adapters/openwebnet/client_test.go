package openwebnet

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"bticino-go-companion/internal/config"
)

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

		buf := make([]byte, 128)
		n, _ := conn.Read(buf)
		if string(buf[:n]) != "*99*0##" {
			done <- errUnexpected(string(buf[:n]))
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))

		n, _ = conn.Read(buf)
		if string(buf[:n]) != "*8*19*21##" {
			done <- errUnexpected(string(buf[:n]))
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))

		n, _ = conn.Read(buf)
		if string(buf[:n]) != "*8*20*21##" {
			done <- errUnexpected(string(buf[:n]))
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

type errUnexpected string

func (e errUnexpected) Error() string { return "unexpected frame: " + string(e) }

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

func TestCommandClientMuteUnmute(t *testing.T) {
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

		if got := reader.readFrame(t); got != "*#8**#33*0##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#8**33*0##"))
		_ = conn.Close()

		conn, err = ln.Accept()
		if err != nil {
			done <- err
			return
		}
		reader = &testFrameReader{conn: conn}
		if got := reader.readFrame(t); got != "*99*0##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))

		if got := reader.readFrame(t); got != "*#8**#33*1##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#8**33*1##"))
		_ = conn.Close()
		done <- nil
	}()

	host, portRaw, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portRaw)
	cfg := config.Default()
	cfg.OpenWebNetCommandHost = host
	cfg.OpenWebNetCommandPort = port
	cfg.OpenWebNetCommandSec = 1

	c := NewCommandClient(cfg)
	if err := c.Mute(context.Background()); err != nil {
		t.Fatalf("mute failed: %v", err)
	}
	if err := c.Unmute(context.Background()); err != nil {
		t.Fatalf("unmute failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server flow failed: %v", err)
	}
}

func TestCommandClientAudioMutedStatus(t *testing.T) {
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

		if got := reader.readFrame(t); got != "*#8**33##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#8**33*0##"))
		done <- nil
	}()

	host, portRaw, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portRaw)
	cfg := config.Default()
	cfg.OpenWebNetCommandHost = host
	cfg.OpenWebNetCommandPort = port
	cfg.OpenWebNetCommandSec = 1

	c := NewCommandClient(cfg)
	muted, err := c.AudioMutedStatus(context.Background())
	if err != nil {
		t.Fatalf("audio status failed: %v", err)
	}
	if !muted {
		t.Fatal("expected muted=true")
	}
	if err := <-done; err != nil {
		t.Fatalf("server flow failed: %v", err)
	}
}

func TestCommandClientVoicemailEnableDisable(t *testing.T) {
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
		if got := reader.readFrame(t); got != "*8*91##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))
		_ = conn.Close()

		conn, err = ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		reader = &testFrameReader{conn: conn}
		if got := reader.readFrame(t); got != "*99*0##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##"))
		if got := reader.readFrame(t); got != "*8*92##" {
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
	if err := c.VoicemailEnable(context.Background()); err != nil {
		t.Fatalf("voicemail enable failed: %v", err)
	}
	if err := c.VoicemailDisable(context.Background()); err != nil {
		t.Fatalf("voicemail disable failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server flow failed: %v", err)
	}
}

func TestCommandClientVoicemailStatus(t *testing.T) {
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

		if got := reader.readFrame(t); got != "*#8**40##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#8**40*1*0*0153*1*25##"))
		done <- nil
	}()

	host, portRaw, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portRaw)
	cfg := config.Default()
	cfg.OpenWebNetCommandHost = host
	cfg.OpenWebNetCommandPort = port
	cfg.OpenWebNetCommandSec = 1

	c := NewCommandClient(cfg)
	status, err := c.VoicemailStatus(context.Background())
	if err != nil {
		t.Fatalf("voicemail status failed: %v", err)
	}
	if !status.Enabled {
		t.Fatal("expected voicemail enabled=true")
	}
	if status.WelcomeMessageEnabled {
		t.Fatal("expected welcome message enabled=false")
	}
	if err := <-done; err != nil {
		t.Fatalf("server flow failed: %v", err)
	}
}

func TestCommandClientDiagnosticSnapshot(t *testing.T) {
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

		if got := reader.readFrame(t); got != "*#13**10##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##*#13**10*192*0*2*172##*#*1##"))

		if got := reader.readFrame(t); got != "*#13**11##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##*#13**11*255*255*255*0##*#*1##"))

		if got := reader.readFrame(t); got != "*#13**12##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##*#13**12*0*17*34*51*68*85##*#*1##"))

		if got := reader.readFrame(t); got != "*#13**16##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##*#13**16*9*8*7##*#*1##"))

		if got := reader.readFrame(t); got != "*#13**17##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##*#13**17*3*2*1##*#*1##"))

		if got := reader.readFrame(t); got != "*#13**23##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##*#13**23*6*1*2##*#*1##"))

		if got := reader.readFrame(t); got != "*#13**24##" {
			done <- errUnexpected(got)
			return
		}
		_, _ = conn.Write([]byte("*#*1##*#13**24*1*2*3##*#*1##"))
		done <- nil
	}()

	host, portRaw, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portRaw)
	cfg := config.Default()
	cfg.OpenWebNetCommandHost = host
	cfg.OpenWebNetCommandPort = port
	cfg.OpenWebNetCommandSec = 1

	c := NewCommandClient(cfg)
	snap, err := c.DiagnosticSnapshot(context.Background())
	if err != nil {
		t.Fatalf("diagnostic snapshot failed: %v", err)
	}
	if snap.IP != "192.0.2.172" {
		t.Fatalf("unexpected ip: %q", snap.IP)
	}
	if snap.Netmask != "255.255.255.0" {
		t.Fatalf("unexpected netmask: %q", snap.Netmask)
	}
	if snap.MAC != "00:11:22:33:44:55" {
		t.Fatalf("unexpected mac: %q", snap.MAC)
	}
	if snap.Firmware != "9.8.7" {
		t.Fatalf("unexpected firmware: %q", snap.Firmware)
	}
	if snap.Hardware != "3.2.1" {
		t.Fatalf("unexpected hardware: %q", snap.Hardware)
	}
	if snap.Kernel != "6.1.2" {
		t.Fatalf("unexpected kernel: %q", snap.Kernel)
	}
	if snap.Distribution != "1.2.3" {
		t.Fatalf("unexpected distribution: %q", snap.Distribution)
	}
	if err := <-done; err != nil {
		t.Fatalf("server flow failed: %v", err)
	}
}

type errUnexpected string

func (e errUnexpected) Error() string { return "unexpected frame: " + string(e) }

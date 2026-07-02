package openwebnet

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"bticino-go-companion/internal/config"
)

type fakeAVServer struct {
	ln      net.Listener
	mu      sync.Mutex
	frames  []string
	replies []string // consumed in order; empty string means "no reply" (client should hit read timeout)
}

func newFakeAVServer(t *testing.T, replies ...string) *fakeAVServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake av listen failed: %v", err)
	}
	s := &fakeAVServer{ln: ln, replies: replies}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeAVServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeAVServer) handle(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 256)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return
			}
			return
		}
		s.mu.Lock()
		s.frames = append(s.frames, string(buf[:n]))
		var reply string
		if len(s.replies) > 0 {
			reply = s.replies[0]
			s.replies = s.replies[1:]
		}
		s.mu.Unlock()
		if reply != "" {
			_, _ = conn.Write([]byte(reply))
		}
	}
}

func (s *fakeAVServer) receivedFrames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.frames...)
}

func newTestAVClient(t *testing.T, s *fakeAVServer, highRes bool) *AVMediaClient {
	t.Helper()
	host, portStr, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr failed: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	cfg := config.Default()
	cfg.MediaAVEndpointHost = host
	cfg.MediaAVEndpointPort = port
	cfg.MediaAVHighResVideo = highRes
	c := NewAVMediaClient(cfg, log.New(io.Discard, "", 0))
	// keep tests fast
	c.retryDelay = 10 * time.Millisecond
	c.audioDelay = 10 * time.Millisecond
	c.replyTimeout = 500 * time.Millisecond
	return c
}

func TestAVMediaClientStreamStartSendsVideoThenAudio(t *testing.T) {
	srv := newFakeAVServer(t, "*#*1##", "*#*1##")
	c := newTestAVClient(t, srv, false)

	if err := c.StreamStart(context.Background(), 5000, 5007); err != nil {
		t.Fatalf("stream start failed: %v", err)
	}
	frames := srv.receivedFrames()
	if len(frames) != 2 {
		t.Fatalf("expected 2 frames, got %d: %v", len(frames), frames)
	}
	if frames[0] != "*7*300#127#0#0#1#5007#1*##" {
		t.Fatalf("unexpected video frame: %s", frames[0])
	}
	if frames[1] != "*7*300#127#0#0#1#5000#2*##" {
		t.Fatalf("unexpected audio frame: %s", frames[1])
	}
}

func TestAVMediaClientHighResVideoFrame(t *testing.T) {
	srv := newFakeAVServer(t, "*#*1##", "*#*1##")
	c := newTestAVClient(t, srv, true)

	if err := c.StreamStart(context.Background(), 5000, 5007); err != nil {
		t.Fatalf("stream start failed: %v", err)
	}
	frames := srv.receivedFrames()
	if frames[0] != "*7*300#127#0#0#1#5007#0*##" {
		t.Fatalf("expected high-res video frame, got %s", frames[0])
	}
}

func TestAVMediaClientToleratesDoubledACK(t *testing.T) {
	srv := newFakeAVServer(t, "*#*1##*#*1##", "*#*1##")
	c := newTestAVClient(t, srv, false)

	if err := c.StreamStart(context.Background(), 5000, 5007); err != nil {
		t.Fatalf("stream start failed with doubled ACK: %v", err)
	}
}

func TestAVMediaClientRetriesOnNAKThenSucceeds(t *testing.T) {
	srv := newFakeAVServer(t, "*#*0##", "*#*1##", "*#*1##")
	c := newTestAVClient(t, srv, false)

	if err := c.StreamStart(context.Background(), 5000, 5007); err != nil {
		t.Fatalf("expected retry to recover from NAK: %v", err)
	}
	frames := srv.receivedFrames()
	if len(frames) != 3 { // video (NAK), video retry (ACK), audio (ACK)
		t.Fatalf("expected 3 frames, got %d: %v", len(frames), frames)
	}
	if frames[0] != frames[1] {
		t.Fatalf("retry should resend the same frame: %q vs %q", frames[0], frames[1])
	}
}

func TestAVMediaClientFailsAfterPersistentNAK(t *testing.T) {
	srv := newFakeAVServer(t, "*#*0##", "*#*0##", "*#*0##")
	c := newTestAVClient(t, srv, false)

	err := c.StreamStart(context.Background(), 5000, 5007)
	if err == nil {
		t.Fatal("expected error after 3 NAKs")
	}
	if !errors.Is(err, ErrAVCommandRejected) {
		t.Fatalf("expected ErrAVCommandRejected, got: %v", err)
	}
	if got := len(srv.receivedFrames()); got != 3 {
		t.Fatalf("expected exactly 3 attempts, got %d", got)
	}
}

func TestAVMediaClientErrorsOnGarbageReply(t *testing.T) {
	srv := newFakeAVServer(t, "*#*banana##", "*#*banana##", "*#*banana##")
	c := newTestAVClient(t, srv, false)

	err := c.StreamStart(context.Background(), 5000, 5007)
	if err == nil {
		t.Fatal("expected error on unsupported reply")
	}
	if !errors.Is(err, ErrAVCommandRejected) {
		t.Fatalf("expected ErrAVCommandRejected, got: %v", err)
	}
}

func TestAVMediaClientTimesOutWithoutReply(t *testing.T) {
	srv := newFakeAVServer(t, "", "", "")
	c := newTestAVClient(t, srv, false)

	start := time.Now()
	err := c.StreamStart(context.Background(), 5000, 5007)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestAVMediaClientRejectsInvalidPorts(t *testing.T) {
	srv := newFakeAVServer(t)
	c := newTestAVClient(t, srv, false)
	if err := c.StreamStart(context.Background(), 0, 5007); err == nil {
		t.Fatal("expected error for invalid audio port")
	}
	if err := c.StreamStart(context.Background(), 5000, 0); err == nil {
		t.Fatal("expected error for invalid video port")
	}
	if got := len(srv.receivedFrames()); got != 0 {
		t.Fatalf("expected no frames sent, got %d", got)
	}
}

func TestAVMediaClientConnectionRefused(t *testing.T) {
	cfg := config.Default()
	cfg.MediaAVEndpointHost = "127.0.0.1"
	cfg.MediaAVEndpointPort = 1 // nothing listens here
	c := NewAVMediaClient(cfg, log.New(io.Discard, "", 0))
	c.retryDelay = 10 * time.Millisecond
	c.dialTimeout = 200 * time.Millisecond
	if err := c.StreamStart(context.Background(), 5000, 5007); err == nil {
		t.Fatal("expected dial error")
	}
}

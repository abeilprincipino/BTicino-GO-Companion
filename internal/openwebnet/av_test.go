package openwebnet

import (
	"bticino-go-companion/internal/media"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// testAVPorts uses distinctive values (neither the old hardcoded 5007/5000 nor
// each other) so a regression back to the literals is caught.
var testAVPorts = media.AVPorts{Video: 61007, Audio: 61553}

func TestAVClientStartsProfiledVideoThenAudio(t *testing.T) {
	server := newAVTestServer(t, FrameACK, FrameACK)
	client := newAVTestClient(server)
	video := &testFlowProbe{}
	audio := &testFlowProbe{}
	server.onFrame = func(count int) {
		if count == 1 {
			video.flowing = true
		}

		if count == 2 {
			audio.flowing = true
		}
	}

	if err := client.Start(context.Background(), true, testAVPorts, video, audio); err != nil {
		t.Fatalf("start: %v", err)
	}

	frames := server.Frames()
	if len(frames) != 2 || frames[0] != "*7*300#127#0#0#1#61007#0*##" || frames[1] != "*7*300#127#0#0#1#61553#2*##" {
		t.Fatalf("frames = %v", frames)
	}
}

func TestAVClientUsesObservedFlowAfterNACK(t *testing.T) {
	server := newAVTestServer(t, FrameNACK, FrameACK)
	client := newAVTestClient(server)
	video := &testFlowProbe{}
	audio := &testFlowProbe{}
	server.onFrame = func(count int) {
		if count == 1 {
			video.flowing = true
		}

		if count == 2 {
			audio.flowing = true
		}
	}

	if err := client.Start(context.Background(), false, testAVPorts, video, audio); err != nil {
		t.Fatalf("start with observed video flow: %v", err)
	}

	frames := server.Frames()
	if len(frames) != 2 || frames[0] != "*7*300#127#0#0#1#61007#1*##" {
		t.Fatalf("frames = %v", frames)
	}
}

func TestAVClientBoundsRejectedAttempts(t *testing.T) {
	server := newAVTestServer(t, FrameNACK)
	client := newAVTestClient(server)

	started := time.Now()

	err := client.Start(context.Background(), false, testAVPorts, &testFlowProbe{}, &testFlowProbe{})
	if !errors.Is(err, ErrAVCommandRejected) {
		t.Fatalf("start error = %v, want rejected command", err)
	}

	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("start gave up after %v, want about the %v retry window", elapsed, client.retryWindow)
	}

	if got := len(server.Frames()); got < 2 {
		t.Fatalf("attempts = %d, want the request repeated within the window", got)
	}
}

// The intercom refuses video until its own stream is up: the request is
// repeated until it is accepted, however many refusals come first.
func TestAVClientRetriesUntilTheIntercomAccepts(t *testing.T) {
	server := newAVTestServer(t, FrameNACK, FrameNACK, FrameNACK, FrameNACK, FrameNACK, FrameACK)
	client := newAVTestClient(server)
	client.retryWindow = 5 * time.Second
	video := &testFlowProbe{}
	server.onFrame = func(count int) {
		if count == 6 {
			video.flowing = true
		}
	}

	if err := client.StartVideo(context.Background(), false, testAVPorts.Video, video); err != nil {
		t.Fatalf("StartVideo after five refusals: %v", err)
	}

	if got := len(server.Frames()); got != 6 {
		t.Fatalf("attempts = %d, want 6", got)
	}
}

type testFlowProbe struct{ flowing bool }

func (p *testFlowProbe) RecentlyFlowing(time.Duration) bool { return p.flowing }

type avTestServer struct {
	listener net.Listener
	mu       sync.Mutex
	frames   []string
	replies  []string
	onFrame  func(int)
}

func newAVTestServer(t *testing.T, replies ...string) *avTestServer {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	server := &avTestServer{listener: listener, replies: replies}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			go server.handle(conn)
		}
	}()

	t.Cleanup(func() { _ = listener.Close() })

	return server
}

func (s *avTestServer) handle(conn net.Conn) {
	defer conn.Close()

	buf := make([]byte, 256)

	n, err := conn.Read(buf)
	if err != nil {
		return
	}

	s.mu.Lock()
	s.frames = append(s.frames, string(buf[:n]))
	count := len(s.frames)
	// The last reply repeats once the others are used up.
	reply := s.replies[0]
	if len(s.replies) > 1 {
		s.replies = s.replies[1:]
	}
	onFrame := s.onFrame
	s.mu.Unlock()

	if onFrame != nil {
		onFrame(count)
	}

	_, _ = conn.Write([]byte(reply))
}

func (s *avTestServer) Frames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.frames...)
}

func newAVTestClient(server *avTestServer) *AVClient {
	_, port, _ := net.SplitHostPort(server.listener.Addr().String())
	client := NewAVClient(nil)
	client.address = net.JoinHostPort("127.0.0.1", port)
	client.retryDelay = time.Millisecond
	client.retryWindow = 50 * time.Millisecond
	client.flowTimeout = 20 * time.Millisecond
	client.flowPoll = time.Millisecond

	return client
}

func TestAVClientStartVideoSendsOnlyTheVideoFrame(t *testing.T) {
	server := newAVTestServer(t, FrameACK)
	client := newAVTestClient(server)
	video := &testFlowProbe{}
	server.onFrame = func(int) { video.flowing = true }

	if err := client.StartVideo(context.Background(), false, testAVPorts.Video, video); err != nil {
		t.Fatalf("StartVideo: %v", err)
	}

	frames := server.Frames()
	if len(frames) != 1 || frames[0] != "*7*300#127#0#0#1#61007#1*##" {
		t.Fatalf("frames = %v, want only the low-res video request", frames)
	}
}

func TestAVClientStartAudioSendsOnlyTheAudioFrame(t *testing.T) {
	server := newAVTestServer(t, FrameACK)
	client := newAVTestClient(server)
	audio := &testFlowProbe{}
	server.onFrame = func(int) { audio.flowing = true }

	if err := client.StartAudio(context.Background(), testAVPorts.Audio, audio); err != nil {
		t.Fatalf("StartAudio: %v", err)
	}

	frames := server.Frames()
	if len(frames) != 1 || frames[0] != "*7*300#127#0#0#1#61553#2*##" {
		t.Fatalf("frames = %v, want only the audio request", frames)
	}
}

func TestAVClientStartAudioBoundsRejectedAttempts(t *testing.T) {
	server := newAVTestServer(t, FrameNACK, FrameNACK, FrameNACK)
	client := newAVTestClient(server)

	err := client.StartAudio(context.Background(), testAVPorts.Audio, &testFlowProbe{})
	if !errors.Is(err, ErrAVCommandRejected) {
		t.Fatalf("StartAudio error = %v, want rejected command", err)
	}
}

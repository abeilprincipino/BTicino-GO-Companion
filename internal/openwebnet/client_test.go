package openwebnet

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestControlInitialEventsReadsAudioAndVoicemailStatus(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	done := make(chan error, 1)
	go func() {
		defer close(done)
		for _, response := range []string{FrameAudioMuted, "*#8**40*1*0##"} {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			reader := &frameReader{conn: conn}
			if frame, err := reader.read(); err != nil || frame != FrameSessionStartCmd {
				_ = conn.Close()
				done <- errUnexpectedFrame(frame, err)
				return
			}
			_, _ = conn.Write([]byte(FrameACK))
			if _, err := reader.read(); err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			_, _ = conn.Write([]byte(response))
			_ = conn.Close()
		}
		done <- nil
	}()

	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	control := NewControl([]config.Entrypoint{{ID: "main", DevAddr: "20"}}, nil)
	control.host = host
	control.port = port
	control.timeout = time.Second

	events, err := control.InitialEvents(context.Background())
	if err != nil {
		t.Fatalf("initial events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if _, ok := events[0].(core.AudioMuted); !ok {
		t.Fatalf("audio event = %T, want core.AudioMuted", events[0])
	}
	if _, ok := events[1].(core.VoicemailEnabled); !ok {
		t.Fatalf("voicemail event = %T, want core.VoicemailEnabled", events[1])
	}
	if err := <-done; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func errUnexpectedFrame(frame string, err error) error {
	if err != nil {
		return err
	}
	return &unexpectedFrameError{frame: frame}
}

type unexpectedFrameError struct{ frame string }

func (e *unexpectedFrameError) Error() string { return "unexpected frame: " + e.frame }

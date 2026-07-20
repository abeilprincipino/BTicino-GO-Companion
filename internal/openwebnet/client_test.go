package openwebnet

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"context"
	"errors"
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

func TestControlEnableVoicemailChecksAvailabilityFirst(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	command := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		reader := &frameReader{conn: conn}
		if frame, err := reader.read(); err != nil || frame != FrameSessionStartCmd {
			done <- errUnexpectedFrame(frame, err)
			return
		}
		_, _ = conn.Write([]byte(FrameACK))
		frame, err := reader.read()
		if err != nil {
			done <- err
			return
		}
		command <- frame
		_, _ = conn.Write([]byte(FrameACK + FrameNACK))
		done <- nil
	}()

	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	control := NewControl(nil, nil)
	control.host = host
	control.port = port
	control.timeout = time.Second
	if err := control.Enable(context.Background()); !errors.Is(err, ErrVoicemailUnavailable) {
		t.Fatalf("Enable() error = %v, want voicemail unavailable", err)
	}
	if frame := <-command; frame != FrameVoicemailStatusCmd {
		t.Fatalf("first command = %q, want voicemail status query", frame)
	}
	if err := <-done; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestControlDiagnosticSnapshotUsesOneSession(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		reader := &frameReader{conn: conn}
		if frame, err := reader.read(); err != nil || frame != FrameSessionStartCmd {
			done <- errUnexpectedFrame(frame, err)
			return
		}
		_, _ = conn.Write([]byte(FrameACK))
		responses := map[string]string{
			FrameDiagIPCmd: "*#13**10*192*0*2*10##", FrameDiagNetmaskCmd: "*#13**11*255*255*255*0##",
			FrameDiagMACCmd: "*#13**12*0*17*34*51*68*85##", FrameDiagFirmwareCmd: "*#13**16*2*3*4##",
			FrameDiagHardwareCmd: "*#13**17*rev*b##", FrameDiagKernelCmd: "*#13**23*6*1##", FrameDiagDistributionCmd: "*#13**24*openwrt##",
		}
		for range responses {
			frame, err := reader.read()
			if err != nil {
				done <- err
				return
			}
			_, _ = conn.Write([]byte(responses[frame]))
		}
		done <- nil
	}()

	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	control := NewControl(nil, nil)
	control.host, control.port, control.timeout = host, port, time.Second
	snapshot, err := control.DiagnosticSnapshot(context.Background())
	if err != nil {
		t.Fatalf("diagnostic snapshot: %v", err)
	}
	if snapshot.IP != "192.0.2.10" || snapshot.MAC != "00:11:22:33:44:55" || snapshot.Firmware != "2.3.4" {
		t.Fatalf("snapshot = %#v", snapshot)
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

package media

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestRTPReceiverParsesExpectedPayloadAndReportsFlow(t *testing.T) {
	ctx := t.Context()

	received := make(chan *rtp.Packet, 1)

	receiver := NewRTPReceiver(RTPReceiverConfig{Address: "127.0.0.1:0", Codec: "H264", PayloadType: VideoPayloadType, Packet: func(packet *rtp.Packet) { received <- packet }})
	if err := receiver.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	packet := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: VideoPayloadType, SequenceNumber: 7, SSRC: 42}, Payload: []byte{1, 2, 3}}

	data, err := packet.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("udp", receiver.Metadata().LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-received:
		if got.SSRC != 42 || got.PayloadType != VideoPayloadType {
			t.Fatalf("packet = %#v", got.Header)
		}
	case <-time.After(time.Second):
		t.Fatal("receiver did not receive RTP")
	}

	metadata := receiver.Metadata()
	if metadata.Codec != "H264" || metadata.PacketCount != 1 || metadata.SSRC != 42 || !receiver.RecentlyFlowing(time.Second) {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestRTPReceiverRejectsUnexpectedPayloadType(t *testing.T) {
	ctx := t.Context()

	receiver := NewRTPReceiver(RTPReceiverConfig{Address: "127.0.0.1:0", Codec: "Speex/8000", PayloadType: AudioPayloadType})
	if err := receiver.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	packet := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: VideoPayloadType, SSRC: 42}}
	data, _ := packet.Marshal()

	conn, err := net.Dial("udp", receiver.Metadata().LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, _ = conn.Write(data)

	time.Sleep(20 * time.Millisecond)

	metadata := receiver.Metadata()
	if metadata.PacketCount != 0 || metadata.InvalidCount != 1 || receiver.RecentlyFlowing(time.Second) {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestNewAudioRTPReceiverBindsEphemeralPort(t *testing.T) {
	ctx := t.Context()

	receiver := NewAudioRTPReceiver(nil, nil)
	if err := receiver.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	port := receiver.Metadata().LocalPort
	if port == 0 || port == 5000 {
		t.Fatalf("bound port = %d, want a non-zero ephemeral port other than the old fixed 5000", port)
	}
}

func TestTwoAudioRTPReceiversStartSimultaneouslyWithoutPortCollision(t *testing.T) {
	ctx := t.Context()

	first := NewAudioRTPReceiver(nil, nil)
	if err := first.Start(ctx); err != nil {
		t.Fatalf("first receiver start: %v", err)
	}
	defer first.Close()

	second := NewAudioRTPReceiver(nil, nil)
	if err := second.Start(ctx); err != nil {
		t.Fatalf("second receiver start: %v", err)
	}
	defer second.Close()

	firstPort := first.Metadata().LocalPort
	secondPort := second.Metadata().LocalPort

	if firstPort == 0 || secondPort == 0 {
		t.Fatalf("bound ports = %d, %d, want both non-zero", firstPort, secondPort)
	}

	if firstPort == secondPort {
		t.Fatalf("both audio receivers bound the same port %d", firstPort)
	}
}

func TestRTPReceiverLogsStructuredBindAndFirstPacketFacts(t *testing.T) {
	ctx := t.Context()

	var logs bytes.Buffer

	received := make(chan struct{}, 1)

	receiver := NewRTPReceiver(RTPReceiverConfig{
		Address:     "127.0.0.1:0",
		Codec:       "H264",
		PayloadType: VideoPayloadType,
		Logger:      slog.New(slog.NewJSONHandler(&logs, nil)),
		Packet:      func(*rtp.Packet) { received <- struct{}{} },
	})
	if err := receiver.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	packet := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: VideoPayloadType, SSRC: 42}, Payload: []byte{1, 2, 3}}

	data, err := packet.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("udp", receiver.Metadata().LocalAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}

	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("receiver did not receive RTP")
	}

	entries := decodeLogEntries(t, logs.Bytes())
	if !hasLogEntry(entries, "rtp receiver bound", map[string]any{"component": "media.rtp", "codec": "H264", "payload_type": float64(VideoPayloadType)}) {
		t.Fatalf("missing structured bind log: %#v", entries)
	}

	if !hasLogEntry(entries, "rtp receiver received first valid packet", map[string]any{"ssrc": float64(42), "local_addr": receiver.Metadata().LocalAddr}) {
		t.Fatalf("missing structured first-packet log: %#v", entries)
	}
}

func decodeLogEntries(t *testing.T, body []byte) []map[string]any {
	t.Helper()

	decoder := json.NewDecoder(bytes.NewReader(body))

	var entries []map[string]any

	for {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			if err == io.EOF {
				break
			}

			t.Fatal(err)
		}

		entries = append(entries, entry)
	}

	return entries
}

func hasLogEntry(entries []map[string]any, message string, want map[string]any) bool {
	for _, entry := range entries {
		if entry["msg"] != message {
			continue
		}

		matches := true

		for key, value := range want {
			if entry[key] != value {
				matches = false
				break
			}
		}

		if matches {
			return true
		}
	}

	return false
}

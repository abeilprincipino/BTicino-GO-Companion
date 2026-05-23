package rtspadapter

import (
	"context"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/domain/entrypoint"
)

type lifecycleRecorder struct {
	joinCalls  int
	leaveCalls int
	touchCalls int
	lastJoinID string
	lastDev    string
}

func (r *lifecycleRecorder) ReaderJoin(_ context.Context, sessionID string, entrypointID string, devAddr string) error {
	r.joinCalls++
	r.lastJoinID = entrypointID
	r.lastDev = devAddr
	return nil
}

func (r *lifecycleRecorder) ReaderLeave(_ context.Context, _ string) error {
	r.leaveCalls++
	return nil
}

func (r *lifecycleRecorder) ReaderTouch(_ string) {
	r.touchCalls++
}

func TestServerPathAndLifecycleHooks(t *testing.T) {
	cfg := config.Default()
	cfg.Entrypoints = []entrypoint.Model{
		{ID: "gate1", DevAddr: "20", HasStream: true},
		{ID: "gate2", DevAddr: "21", HasStream: true},
	}

	rec := &lifecycleRecorder{}
	s := NewServer(cfg, log.New(io.Discard, "", 0), rec)

	if !s.isKnownPath("/doorbell-gate1") || !s.isKnownPath("/doorbell-gate2") || s.isKnownPath("/doorbell") || s.isKnownPath("/unknown") {
		t.Fatal("unexpected path match behavior")
	}

	session := &gortsplib.ServerSession{}
	playResp, err := s.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{
		Path:    "/doorbell-gate2",
		Session: session,
	})
	if err != nil {
		t.Fatalf("on play returned error: %v", err)
	}
	if playResp == nil || playResp.StatusCode != base.StatusOK {
		t.Fatalf("unexpected play response: %+v", playResp)
	}
	if rec.joinCalls != 1 || rec.lastJoinID != "gate2" || rec.lastDev != "21" {
		t.Fatalf("unexpected lifecycle join payload: %+v", rec)
	}

	pauseResp, err := s.OnPause(&gortsplib.ServerHandlerOnPauseCtx{
		Session: session,
	})
	if err != nil {
		t.Fatalf("on pause returned error: %v", err)
	}
	if pauseResp == nil || pauseResp.StatusCode != base.StatusOK {
		t.Fatalf("unexpected pause response: %+v", pauseResp)
	}
	if rec.leaveCalls != 1 {
		t.Fatalf("expected one leave callback, got %d", rec.leaveCalls)
	}
}

func TestServerDescribeUnknownPath(t *testing.T) {
	cfg := config.Default()
	rec := &lifecycleRecorder{}
	s := NewServer(cfg, log.New(io.Discard, "", 0), rec)

	resp, _, err := s.OnDescribe(&gortsplib.ServerHandlerOnDescribeCtx{Path: "/unknown"})
	if err != nil {
		t.Fatalf("on describe returned error: %v", err)
	}
	if resp == nil || resp.StatusCode != base.StatusNotFound {
		t.Fatalf("unexpected describe response: %+v", resp)
	}
}

func TestServerHelperFunctions(t *testing.T) {
	cfg := config.Default()
	cfg.Entrypoints = []entrypoint.Model{
		{ID: "main", DevAddr: "20", HasStream: true},
	}
	s := NewServer(cfg, log.New(io.Discard, "", 0), &lifecycleRecorder{})

	if id, dev, ok := s.resolveEntrypoint("/doorbell-main"); !ok || id != "main" || dev != "20" {
		t.Fatalf("unexpected resolveEntrypoint result: id=%q dev=%q ok=%v", id, dev, ok)
	}
	if _, _, ok := s.resolveEntrypoint("/missing"); ok {
		t.Fatal("expected unresolved entrypoint for missing path")
	}

	if !isExpectedPayloadType(description.MediaTypeVideo, 96) {
		t.Fatal("expected video payload type 96 to match")
	}
	if isExpectedPayloadType(description.MediaTypeVideo, 110) {
		t.Fatal("did not expect video payload type 110 to match")
	}
	if !isExpectedPayloadType(description.MediaTypeAudio, 110) {
		t.Fatal("expected audio payload type 110 to match")
	}
	if isExpectedPayloadType(description.MediaTypeAudio, 97) {
		t.Fatal("did not expect return audio payload type 97 on ingest")
	}
	if isExpectedPayloadType(description.MediaTypeAudio, 96) {
		t.Fatal("did not expect audio payload type 96 to match")
	}

	got := sortedRoutePaths(map[string]entrypoint.StreamRoute{
		"doorbell-b": {},
		"doorbell-a": {},
	})
	if len(got) != 2 || got[0] != "doorbell-a" || got[1] != "doorbell-b" {
		t.Fatalf("unexpected sorted paths: %+v", got)
	}

	s.touchReader(nil)
	s.removeReader(nil)
	s.writeIngestPacket(description.MediaTypeVideo, nil)

	if id := sessionID(&gortsplib.ServerSession{}); id == "" {
		t.Fatal("expected non-empty session id")
	}
}

func TestServerStaticStreamIncludesSpeexBackchannel(t *testing.T) {
	desc, _, _, backMed := buildStaticStreamDescription()

	if backMed == nil || len(desc.Medias) != 3 {
		t.Fatalf("expected direct video/audio plus backchannel, got %+v", desc.Medias)
	}
	if !backMed.IsBackChannel || backMed.Type != description.MediaTypeAudio {
		t.Fatalf("unexpected backchannel media: %+v", backMed)
	}
	speex, ok := backMed.Formats[0].(*format.Speex)
	if !ok {
		t.Fatalf("expected Speex backchannel format, got %T", backMed.Formats[0])
	}
	if speex.PayloadTyp != rtpPayloadTypeSpeexBackchannel || speex.SampleRate != rtpSpeexSampleRate {
		t.Fatalf("unexpected backchannel speex format: %+v", speex)
	}

	marshaled, err := desc.Marshal()
	if err != nil {
		t.Fatalf("marshal description failed: %v", err)
	}
	sdp := string(marshaled)
	if !strings.Contains(sdp, "m=audio 0 RTP/AVP 97") || !strings.Contains(sdp, "a=sendonly") {
		t.Fatalf("expected Speex backchannel in SDP: %s", sdp)
	}
}

func TestReturnAudioForwarderWritesRTP(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer conn.Close()

	forwarder := newReturnAudioForwarder(conn.LocalAddr().String())
	defer forwarder.Close()

	want := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    rtpPayloadTypeSpeexBackchannel,
			SequenceNumber: 123,
			Timestamp:      456,
			SSRC:           789,
		},
		Payload: []byte{1, 2, 3, 4},
	}
	if err := forwarder.WriteRTP(want); err != nil {
		t.Fatalf("WriteRTP failed: %v", err)
	}

	buf := make([]byte, 1500)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read udp: %v", err)
	}

	var got rtp.Packet
	if err := got.Unmarshal(buf[:n]); err != nil {
		t.Fatalf("unmarshal rtp: %v", err)
	}
	if got.PayloadType != want.PayloadType || got.SequenceNumber != want.SequenceNumber || got.Timestamp != want.Timestamp || got.SSRC != want.SSRC {
		t.Fatalf("unexpected forwarded RTP header: %+v", got.Header)
	}
	if string(got.Payload) != string(want.Payload) {
		t.Fatalf("unexpected forwarded RTP payload: %+v", got.Payload)
	}
}

func TestServerForwardsOnlyActiveBackchannelRTP(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer conn.Close()

	cfg := config.Default()
	s := NewServer(cfg, log.New(io.Discard, "", 0), &lifecycleRecorder{})
	s.returnAudio = newReturnAudioForwarder(conn.LocalAddr().String())
	defer s.closeReturnAudio()

	session := &gortsplib.ServerSession{}
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:     2,
			PayloadType: rtpPayloadTypeSpeexBackchannel,
		},
		Payload: []byte{1},
	}

	s.forwardReturnAudio(session, pkt)
	if err := conn.SetReadDeadline(time.Now().Add(25 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if n, _, err := conn.ReadFromUDP(make([]byte, 1500)); err == nil || n != 0 {
		t.Fatal("unexpected packet for inactive reader")
	}

	s.readers[session] = readerInfo{SessionID: "s1", EntrypointID: "main", DevAddr: "20"}
	s.forwardReturnAudio(session, pkt)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if n, _, err := conn.ReadFromUDP(make([]byte, 1500)); err != nil || n == 0 {
		t.Fatalf("expected forwarded packet, n=%d err=%v", n, err)
	}
}

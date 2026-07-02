package rtspadapter

import (
	"bytes"
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
	"bticino-go-companion/internal/services/audiobridge"
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

func TestServerOnPlayIsIdempotentPerSession(t *testing.T) {
	cfg := config.Default()
	cfg.Entrypoints = []entrypoint.Model{
		{ID: "gate1", DevAddr: "20", HasStream: true},
	}

	rec := &lifecycleRecorder{}
	s := NewServer(cfg, log.New(io.Discard, "", 0), rec)
	session := &gortsplib.ServerSession{}

	if _, err := s.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{Path: "/doorbell-gate1", Session: session}); err != nil {
		t.Fatalf("on play gate1 first call failed: %v", err)
	}
	if _, err := s.OnPlay(&gortsplib.ServerHandlerOnPlayCtx{Path: "/doorbell-gate1", Session: session}); err != nil {
		t.Fatalf("on play gate1 second call failed: %v", err)
	}
	if rec.joinCalls != 1 {
		t.Fatalf("expected one lifecycle join for idempotent session play, got %d", rec.joinCalls)
	}

	if _, err := s.OnPause(&gortsplib.ServerHandlerOnPauseCtx{Session: session}); err != nil {
		t.Fatalf("on pause gate1 failed: %v", err)
	}
	if rec.leaveCalls != 1 {
		t.Fatalf("expected one lifecycle leave, got %d", rec.leaveCalls)
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

func TestServerStaticStreamIncludesOpusBackchannel(t *testing.T) {
	desc, _, _, backMed := buildStaticStreamDescription(true, 111, 112)

	if backMed == nil || len(desc.Medias) != 3 {
		t.Fatalf("expected direct video/audio plus backchannel, got %+v", desc.Medias)
	}
	if !backMed.IsBackChannel || backMed.Type != description.MediaTypeAudio {
		t.Fatalf("unexpected backchannel media: %+v", backMed)
	}
	opus, ok := backMed.Formats[0].(*format.Opus)
	if !ok {
		t.Fatalf("expected Opus backchannel format, got %T", backMed.Formats[0])
	}
	if opus.PayloadTyp != 112 {
		t.Fatalf("unexpected backchannel opus format: %+v", opus)
	}

	marshaled, err := desc.Marshal()
	if err != nil {
		t.Fatalf("marshal description failed: %v", err)
	}
	sdp := string(marshaled)
	if !strings.Contains(sdp, "m=audio 0 RTP/AVP 112") || !strings.Contains(sdp, "a=sendonly") {
		t.Fatalf("expected Opus backchannel in SDP: %s", sdp)
	}
}

func TestServerStaticStreamIncludesLegacySpeexBackchannel(t *testing.T) {
	desc, _, _, backMed := buildStaticStreamDescription(false, 111, 112)

	if backMed == nil || len(desc.Medias) != 3 {
		t.Fatalf("expected direct video/audio plus backchannel, got %+v", desc.Medias)
	}
	speex, ok := backMed.Formats[0].(*format.Speex)
	if !ok {
		t.Fatalf("expected Speex backchannel format, got %T", backMed.Formats[0])
	}
	if speex.PayloadTyp != rtpPayloadTypeSpeexBackchannel || speex.SampleRate != rtpSpeexSampleRate {
		t.Fatalf("unexpected backchannel speex format: %+v", speex)
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
	s.audioBridge = nil
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

func TestServerWritesBackchannelToBridgeWhenEnabled(t *testing.T) {
	cfg := config.Default()
	s := NewServer(cfg, log.New(io.Discard, "", 0), &lifecycleRecorder{})
	bridgeCfg := audiobridge.DefaultConfig(cfg.DataDir)
	bridgeCfg.Enabled = true
	ports := bridgeCfg.Ports

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ports.OpusIn})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer conn.Close()

	s.audioBridge = audiobridge.New(bridgeCfg, log.New(io.Discard, "", 0))
	session := &gortsplib.ServerSession{}
	s.readers[session] = readerInfo{SessionID: "s1", EntrypointID: "main", DevAddr: "20"}

	want := &rtp.Packet{
		Header: rtp.Header{
			Version:     2,
			PayloadType: s.audioBridge.BackchannelOpusPayloadType(),
		},
		Payload: []byte{9, 8, 7},
	}
	s.forwardReturnAudio(session, want)

	buf := make([]byte, 1500)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("expected bridged packet: %v", err)
	}
	var got rtp.Packet
	if err := got.Unmarshal(buf[:n]); err != nil {
		t.Fatalf("unmarshal packet: %v", err)
	}
	if got.PayloadType != want.PayloadType {
		t.Fatalf("unexpected payload type got=%d want=%d", got.PayloadType, want.PayloadType)
	}
}

func TestInspectH264NALTypes(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		sps     bool
		pps     bool
		idr     bool
	}{
		{
			name:    "single_nal_sps",
			payload: []byte{0x67},
			sps:     true,
		},
		{
			name:    "single_nal_pps",
			payload: []byte{0x68},
			pps:     true,
		},
		{
			name:    "single_nal_idr",
			payload: []byte{0x65},
			idr:     true,
		},
		{
			name:    "stap_a_sps_pps_idr",
			payload: []byte{0x78, 0x00, 0x01, 0x67, 0x00, 0x01, 0x68, 0x00, 0x01, 0x65},
			sps:     true,
			pps:     true,
			idr:     true,
		},
		{
			name:    "fu_a_start_idr",
			payload: []byte{0x7c, 0x85, 0x01},
			idr:     true,
		},
		{
			name:    "fu_a_non_start_idr",
			payload: []byte{0x7c, 0x05, 0x01},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sps, pps, idr := inspectH264NALTypes(tc.payload)
			if sps != tc.sps || pps != tc.pps || idr != tc.idr {
				t.Fatalf("unexpected parse flags got sps=%v pps=%v idr=%v", sps, pps, idr)
			}
		})
	}
}

func TestSnapshotMirrorWaitsForWarmupAndIDR(t *testing.T) {
	cfg := config.Default()
	s := NewServer(cfg, log.New(io.Discard, "", 0), &lifecycleRecorder{})

	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer listener.Close()

	dst := listener.LocalAddr().(*net.UDPAddr)
	conn, err := net.DialUDP("udp4", nil, dst)
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer conn.Close()

	s.snapshotMirrorConn = conn
	s.snapshotMirrorReady = false
	s.snapshotMirrorSPS = false
	s.snapshotMirrorPPS = false
	s.snapshotMirrorWarmupDoneFrames = 0
	s.snapshotMirrorWaitForIDR = false

	write := func(payload []byte, marker bool) {
		s.writeSnapshotMirror(&rtp.Packet{
			Header: rtp.Header{
				Version:     2,
				PayloadType: rtpPayloadTypeH264,
				Marker:      marker,
			},
			Payload: payload,
		})
	}

	write([]byte{0x61, 0x01}, true)
	if err := listener.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if n, _, err := listener.ReadFromUDP(make([]byte, 1500)); err == nil || n != 0 {
		t.Fatal("unexpected packet before readiness")
	}

	write([]byte{0x67}, false)
	write([]byte{0x68}, false)
	for i := 0; i < snapshotWarmupFrames; i++ {
		write([]byte{0x61, 0x01}, true)
	}

	if err := listener.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if n, _, err := listener.ReadFromUDP(make([]byte, 1500)); err == nil || n != 0 {
		t.Fatal("unexpected packet before post-warmup IDR")
	}

	write([]byte{0x7c, 0x85, 0x01}, false)
	write([]byte{0x7c, 0x45, 0x02}, true)

	buf := make([]byte, 1500)
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	n, _, err := listener.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("expected mirrored packet after readiness: %v", err)
	}
	var pkt rtp.Packet
	if err := pkt.Unmarshal(buf[:n]); err != nil {
		t.Fatalf("unmarshal mirrored rtp: %v", err)
	}
	if pkt.PayloadType != rtpPayloadTypeH264 {
		t.Fatalf("unexpected payload type: %d", pkt.PayloadType)
	}
	if len(pkt.Payload) < 2 || pkt.Payload[0] != 0x7c || pkt.Payload[1] != 0x85 {
		t.Fatalf("unexpected mirrored payload: %v", pkt.Payload)
	}
}

func TestIngestFirstPacketIsLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Default()
	s := NewServer(cfg, log.New(&buf, "", 0), &lifecycleRecorder{})

	pkt := &rtp.Packet{Header: rtp.Header{PayloadType: rtpPayloadTypeH264, SSRC: 42}}
	s.noteIngestPacket(description.MediaTypeVideo, 100, pkt, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999})
	s.noteIngestPacket(description.MediaTypeVideo, 100, pkt, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999})

	out := buf.String()
	if got := strings.Count(out, "first packet"); got != 1 {
		t.Fatalf("expected exactly one first-packet log, got %d in: %s", got, out)
	}
	if !strings.Contains(out, "ssrc=42") {
		t.Fatalf("first-packet log must include SSRC: %s", out)
	}
}

func TestUnexpectedPayloadTypeLoggedOncePerPT(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Default()
	s := NewServer(cfg, log.New(&buf, "", 0), &lifecycleRecorder{})

	bad := &rtp.Packet{Header: rtp.Header{PayloadType: 33}}
	s.writeIngestPacket(description.MediaTypeVideo, bad)
	s.writeIngestPacket(description.MediaTypeVideo, bad)

	out := buf.String()
	if got := strings.Count(out, "unexpected payload type"); got != 1 {
		t.Fatalf("expected exactly one bad-PT log, got %d in: %s", got, out)
	}
}

func TestIngestWatchWarnsWhenNoRTPArrives(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Default()
	s := NewServer(cfg, log.New(&buf, "", 0), &lifecycleRecorder{})
	s.ingestWatchDelay = 20 * time.Millisecond

	s.armIngestWatch()
	time.Sleep(100 * time.Millisecond)

	if !strings.Contains(buf.String(), "no RTP ingress") {
		t.Fatalf("expected no-RTP warning, got: %s", buf.String())
	}
}

func TestIngestWatchSilentWhenRTPArrives(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Default()
	s := NewServer(cfg, log.New(&buf, "", 0), &lifecycleRecorder{})
	s.ingestWatchDelay = 50 * time.Millisecond

	s.armIngestWatch()
	pkt := &rtp.Packet{Header: rtp.Header{PayloadType: rtpPayloadTypeH264}}
	s.noteIngestPacket(description.MediaTypeVideo, 100, pkt, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999})
	time.Sleep(120 * time.Millisecond)

	if strings.Contains(buf.String(), "no RTP ingress") {
		t.Fatalf("did not expect no-RTP warning, got: %s", buf.String())
	}
}

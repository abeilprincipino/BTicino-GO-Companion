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

func TestWriteIngestPacketCountsEgress(t *testing.T) {
	cfg := config.Default()
	s := NewServer(cfg, log.New(io.Discard, "", 0), &lifecycleRecorder{})

	forwarded := 0
	s.SetOnVideoPacketRTP(func(*rtp.Packet) { forwarded++ })

	pkt := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: rtpPayloadTypeH264}}
	s.writeIngestPacket(description.MediaTypeVideo, pkt)
	s.writeIngestPacket(description.MediaTypeVideo, pkt)

	// A packet with an unexpected payload type is dropped before any egress.
	s.writeIngestPacket(description.MediaTypeVideo, &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 33}})

	s.ingestMu.Lock()
	webrtcVideo := s.egressWebRTC[description.MediaTypeVideo]
	rtspVideo := s.egressRTSP[description.MediaTypeVideo]
	s.ingestMu.Unlock()

	if forwarded != 2 {
		t.Fatalf("expected 2 video callback forwards, got %d", forwarded)
	}
	if webrtcVideo != 2 {
		t.Fatalf("expected egress_webrtc_video=2, got %d", webrtcVideo)
	}
	// The static stream is nil (server not started): only actual successful
	// WritePacketRTP calls count as RTSP egress, so the counter must stay 0.
	if rtspVideo != 0 {
		t.Fatalf("expected egress_rtsp_video=0 with nil stream, got %d", rtspVideo)
	}
}

func TestRunBridgeOpusOutListenerCountsEgress(t *testing.T) {
	cfg := config.Default()
	s := NewServer(cfg, log.New(io.Discard, "", 0), &lifecycleRecorder{})

	const opusPayloadType = 111

	forwarded := 0
	s.SetOnAudioPacketRTP(func(*rtp.Packet) { forwarded++ })

	port, err := reserveLocalUDPPort()
	if err != nil {
		t.Fatalf("reserve local udp port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		s.runBridgeOpusOutListener(ctx, port, opusPayloadType)
		close(done)
	}()

	// Give the listener goroutine time to bind before sending.
	time.Sleep(50 * time.Millisecond)

	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer conn.Close()

	send := func(pt uint8) {
		pkt := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: pt}}
		raw, err := pkt.Marshal()
		if err != nil {
			t.Fatalf("marshal packet: %v", err)
		}
		if _, err := conn.Write(raw); err != nil {
			t.Fatalf("write udp: %v", err)
		}
	}

	send(opusPayloadType)
	send(opusPayloadType)
	// A packet with an unexpected payload type must be ignored entirely.
	send(opusPayloadType + 1)

	deadline := time.Now().Add(2 * time.Second)
	for {
		s.ingestMu.Lock()
		got := s.egressWebRTC[description.MediaTypeAudio]
		s.ingestMu.Unlock()
		if got >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for webrtc egress count, got %d", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if forwarded != 2 {
		t.Fatalf("expected 2 audio callback forwards, got %d", forwarded)
	}

	s.ingestMu.Lock()
	webrtcAudio := s.egressWebRTC[description.MediaTypeAudio]
	// The static stream is nil (server not started via Start()): only actual
	// successful WritePacketRTP calls count as RTSP egress, so it must stay 0.
	rtspAudio := s.egressRTSP[description.MediaTypeAudio]
	s.ingestMu.Unlock()

	if webrtcAudio != 2 {
		t.Fatalf("expected egress_webrtc_audio=2, got %d", webrtcAudio)
	}
	if rtspAudio != 0 {
		t.Fatalf("expected egress_rtsp_audio=0 with nil stream, got %d", rtspAudio)
	}

	cancel()
	<-done
}

func TestBridgeIngestFailureLogRateLimit(t *testing.T) {
	cfg := config.Default()
	s := NewServer(cfg, log.New(io.Discard, "", 0), &lifecycleRecorder{})

	base := time.Unix(1000, 0)

	// First failure logs immediately, nothing suppressed yet.
	if logNow, suppressed := s.noteBridgeIngestFailure(base); !logNow || suppressed != 0 {
		t.Fatalf("first failure should log immediately, got logNow=%v suppressed=%d", logNow, suppressed)
	}

	// Rapid subsequent failures within the 5s window are suppressed.
	for i := 1; i <= 100; i++ {
		if logNow, _ := s.noteBridgeIngestFailure(base.Add(time.Duration(i) * time.Millisecond)); logNow {
			t.Fatalf("failure #%d within window should be suppressed", i)
		}
	}

	// After 5s, a summary is emitted reporting how many were suppressed since
	// the last emit (100 failures were suppressed between first-log and now).
	// 101 failures were swallowed since the first log: the 100 mid-window ones
	// plus this one at the 5s boundary that triggers the summary.
	logNow, suppressed := s.noteBridgeIngestFailure(base.Add(5 * time.Second))
	if !logNow {
		t.Fatal("expected a summary log after 5s window")
	}
	if suppressed != 101 {
		t.Fatalf("expected 101 suppressed since last emit, got %d", suppressed)
	}

	// Further failures right after the summary are suppressed again.
	if logNow, _ := s.noteBridgeIngestFailure(base.Add(5*time.Second + time.Millisecond)); logNow {
		t.Fatal("failure right after summary should be suppressed")
	}

	// A success within the quiet period after the last failure does NOT report
	// recovery — writes flap under UDP ICMP semantics.
	if recovered, _ := s.noteBridgeIngestSuccess(base.Add(5*time.Second + 100*time.Millisecond)); recovered {
		t.Fatal("did not expect recovery within the quiet period after a failure")
	}

	// A success reports recovery once, but only after the quiet period elapses
	// with no further failures. Last failure was at base+5s+1ms.
	recovered, failures := s.noteBridgeIngestSuccess(base.Add(5*time.Second + time.Millisecond + bridgeRecoveryQuietPeriod))
	if !recovered {
		t.Fatal("expected recovery to be reported after the quiet period")
	}
	if failures != 103 { // 1 first + 100 mid + 1 summary + 1 after-summary
		t.Fatalf("expected 103 total failures, got %d", failures)
	}

	// A second success without intervening failures reports nothing.
	if recovered, _ := s.noteBridgeIngestSuccess(base.Add(10 * time.Second)); recovered {
		t.Fatal("did not expect a second recovery report")
	}

	// After recovery, the next failure logs immediately again (state reset).
	if logNow, suppressed := s.noteBridgeIngestFailure(base.Add(time.Minute)); !logNow || suppressed != 0 {
		t.Fatalf("post-recovery failure should log immediately, got logNow=%v suppressed=%d", logNow, suppressed)
	}
}

func TestBackchannelCountersAndFirstPacketLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Default()
	s := NewServer(cfg, log.New(&buf, "", 0), &lifecycleRecorder{})

	inPkt := &rtp.Packet{Header: rtp.Header{PayloadType: rtpPayloadTypeSpeexBackchannel, SSRC: 11}}
	outPkt := &rtp.Packet{Header: rtp.Header{PayloadType: rtpPayloadTypeSpeexBackchannel, SSRC: 22}}

	s.noteBackchannelIn(inPkt)
	s.noteBackchannelIn(inPkt)
	s.noteBackchannelOut(outPkt)
	s.noteBackchannelOut(outPkt)
	s.noteBackchannelOut(outPkt)

	s.ingestMu.Lock()
	in := s.backchannelIn
	out := s.backchannelOut
	s.ingestMu.Unlock()
	if in != 2 {
		t.Fatalf("expected backchannel_in=2, got %d", in)
	}
	if out != 3 {
		t.Fatalf("expected backchannel_out=3, got %d", out)
	}

	log := buf.String()
	if got := strings.Count(log, "backchannel first in packet"); got != 1 {
		t.Fatalf("expected exactly one first-in log, got %d in: %s", got, log)
	}
	if got := strings.Count(log, "backchannel first out packet"); got != 1 {
		t.Fatalf("expected exactly one first-out log, got %d in: %s", got, log)
	}
	if !strings.Contains(log, "ssrc=11") || !strings.Contains(log, "ssrc=22") {
		t.Fatalf("expected first-packet logs to include SSRC: %s", log)
	}
}

func TestForwardReturnAudioCountsBackchannel(t *testing.T) {
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
	s.readers[session] = readerInfo{SessionID: "s1", EntrypointID: "main", DevAddr: "20"}
	pkt := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: rtpPayloadTypeSpeexBackchannel}, Payload: []byte{1}}

	s.forwardReturnAudio(session, pkt)
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if n, _, err := conn.ReadFromUDP(make([]byte, 1500)); err != nil || n == 0 {
		t.Fatalf("expected forwarded packet, n=%d err=%v", n, err)
	}

	s.ingestMu.Lock()
	in := s.backchannelIn
	out := s.backchannelOut
	s.ingestMu.Unlock()
	if in != 1 {
		t.Fatalf("expected backchannel_in=1 via forwardReturnAudio, got %d", in)
	}
	if out != 1 {
		t.Fatalf("expected backchannel_out=1 via forwardReturnAudio, got %d", out)
	}
}

func TestWriteBackchannelOpusCountsIngress(t *testing.T) {
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

	pkt := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96}, Payload: []byte{1}}
	if err := s.WriteBackchannelOpus(pkt); err != nil {
		t.Fatalf("WriteBackchannelOpus failed: %v", err)
	}

	s.ingestMu.Lock()
	in := s.backchannelIn
	out := s.backchannelOut
	s.ingestMu.Unlock()
	if in != 1 {
		t.Fatalf("expected backchannel_in=1 via WriteBackchannelOpus, got %d", in)
	}
	// Non-bridge WriteBackchannelOpus writes straight to 127.0.0.1:4000, so Out counts too.
	if out != 1 {
		t.Fatalf("expected backchannel_out=1 via WriteBackchannelOpus, got %d", out)
	}
}

func TestBridgeIngestRecoveryDebouncedUnderFlapping(t *testing.T) {
	cfg := config.Default()
	s := NewServer(cfg, log.New(io.Discard, "", 0), &lifecycleRecorder{})

	base := time.Unix(2000, 0)

	failLogs := 0
	summaryLogs := 0
	recoveryLogs := 0

	noteFail := func(at time.Time) {
		if logNow, suppressed := s.noteBridgeIngestFailure(at); logNow {
			if suppressed > 0 {
				summaryLogs++
			} else {
				failLogs++
			}
		}
	}
	noteSuccess := func(at time.Time) {
		if recovered, _ := s.noteBridgeIngestSuccess(at); recovered {
			recoveryLogs++
		}
	}

	// First failure logs immediately.
	noteFail(base)

	// Flapping: writes alternate success/failure at ~100ms cadence. No success
	// is far enough from the last failure to be a recovery, and every failure
	// falls within the 5s summary window (suppressed, feeding the counter).
	for i := 1; i <= 10; i++ {
		at := base.Add(time.Duration(i) * 100 * time.Millisecond)
		noteSuccess(at)
		noteFail(at)
	}

	// A failure at the 5s boundary emits a single summary line.
	noteFail(base.Add(5 * time.Second))

	// The bridge comes back: writes now succeed. Recovery is declared once, only
	// after the quiet period from the last failure (base+5s).
	noteSuccess(base.Add(5*time.Second + 500*time.Millisecond)) // within quiet period: no recovery
	noteSuccess(base.Add(5*time.Second + bridgeRecoveryQuietPeriod))
	noteSuccess(base.Add(5*time.Second + bridgeRecoveryQuietPeriod + time.Second))

	if failLogs != 1 {
		t.Fatalf("expected exactly ONE initial failure log, got %d", failLogs)
	}
	if summaryLogs != 1 {
		t.Fatalf("expected exactly ONE 5s summary log, got %d", summaryLogs)
	}
	if recoveryLogs != 1 {
		t.Fatalf("expected exactly ONE recovery log after the quiet period, got %d", recoveryLogs)
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

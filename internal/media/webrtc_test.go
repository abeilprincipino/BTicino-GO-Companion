package media

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestWebRTCServiceOfferUsesCoordinatorLeaseAndCloseIsIdempotent(t *testing.T) {
	source := &remoteBYESource{}
	coordinator := NewStreamCoordinator(nil, func(_ config.Entrypoint, events SourceEvents) (ManagedSource, func(), error) {
		source.callback = events.RemoteBYE
		return source, nil, nil
	})

	service := newTestWebRTCService(t, coordinator, []config.Entrypoint{{ID: "main", Capabilities: config.Capabilities{Stream: true}}})

	address, ok := service.iceConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatal("ICE local address is not UDP")
	}

	if port := address.Port; port != webrtcICEPort {
		t.Fatalf("ICE UDP port = %d, want %d", port, webrtcICEPort)
	}

	offer, client := testWebRTCOffer(t)
	defer client.Close()

	if err := service.AddICECandidate("session-1", ICECandidate{Candidate: "candidate:1 1 udp 2130706431 192.0.2.1 12345 typ host"}); err != nil {
		t.Fatalf("AddICECandidate() before offer error = %v", err)
	}

	candidates := make(chan ICECandidate, 8)

	answer, err := service.Offer(context.Background(), "session-1", "main", offer, nil, func(candidate *ICECandidate) {
		if candidate == nil {
			return
		}

		candidates <- *candidate
	})
	if err != nil {
		t.Fatalf("Offer() error = %v, offer = %q", err, offer)
	}

	if strings.TrimSpace(answer) == "" {
		t.Fatal("answer is empty")
	}

	if !strings.Contains(answer, "a=candidate:") {
		t.Fatalf("answer does not contain a local ICE candidate: %q", answer)
	}

	select {
	case candidate := <-candidates:
		if candidate.Candidate == "" {
			t.Fatal("local ICE candidate is empty")
		}
	case <-time.After(time.Second):
		t.Fatal("local ICE candidate was not delivered")
	}

	if coordinator.Snapshot().Owner != StreamOwnerCompanion {
		t.Fatalf("owner = %q, want companion", coordinator.Snapshot().Owner)
	}

	if _, queued := service.pendingCandidates["session-1"]; queued {
		t.Fatal("pre-offer candidates were not consumed")
	}

	if err := service.Close("session-1"); err != nil {
		t.Fatal(err)
	}

	if err := service.Close("session-1"); err != nil {
		t.Fatal(err)
	}

	if source.closes != 1 || coordinator.Snapshot().Owner != StreamOwnerIdle {
		t.Fatalf("closes=%d snapshot=%#v", source.closes, coordinator.Snapshot())
	}
}

func TestWebRTCServiceShutdownClosesICEAndRejectsOffers(t *testing.T) {
	service := newTestWebRTCService(t, NewStreamCoordinator(nil, testManagedSourceFactory()), []config.Entrypoint{{ID: "main", Capabilities: config.Capabilities{Stream: true}}})
	if err := service.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	if _, err := service.Offer(context.Background(), "session-1", "main", "offer", nil, nil); !errors.Is(err, ErrWebRTCClosed) {
		t.Fatalf("Offer() error = %v, want %v", err, ErrWebRTCClosed)
	}
}

func TestWebRTCServiceOfferReplacesPreviousSessionForEntrypoint(t *testing.T) {
	source := &remoteBYESource{}
	coordinator := NewStreamCoordinator(nil, func(_ config.Entrypoint, events SourceEvents) (ManagedSource, func(), error) {
		source.callback = events.RemoteBYE
		return source, nil, nil
	})
	service := newTestWebRTCService(t, coordinator, []config.Entrypoint{{ID: "main", Capabilities: config.Capabilities{Stream: true}}})

	firstOffer, firstClient := testWebRTCOffer(t)
	defer firstClient.Close()

	if _, err := service.Offer(context.Background(), "session-1", "main", firstOffer, nil, nil); err != nil {
		t.Fatalf("offer first session: %v", err)
	}

	secondOffer, secondClient := testWebRTCOffer(t)
	defer secondClient.Close()

	if _, err := service.Offer(context.Background(), "session-2", "main", secondOffer, nil, nil); err != nil {
		t.Fatalf("offer replacement session: %v", err)
	}

	service.mu.Lock()
	_, firstExists := service.sessions["session-1"]
	_, secondExists := service.sessions["session-2"]
	service.mu.Unlock()

	if firstExists || !secondExists {
		t.Fatalf("sessions after replacement: first=%t second=%t", firstExists, secondExists)
	}

	if source.closes != 1 || coordinator.Snapshot().Owner != StreamOwnerCompanion {
		t.Fatalf("closes=%d snapshot=%#v", source.closes, coordinator.Snapshot())
	}
}

func TestWebRTCServiceRejectsUnknownEntrypointWithoutLease(t *testing.T) {
	coordinator := NewStreamCoordinator(nil, testManagedSourceFactory())
	service := newTestWebRTCService(t, coordinator, []config.Entrypoint{{ID: "main", Capabilities: config.Capabilities{Stream: true}}})

	_, err := service.Offer(context.Background(), "session-1", "missing", "offer", nil, nil)
	if !errors.Is(err, ErrEntrypointNotFound) {
		t.Fatalf("Offer() error = %v", err)
	}

	if coordinator.Snapshot().Owner != StreamOwnerIdle {
		t.Fatalf("snapshot = %#v", coordinator.Snapshot())
	}
}

func TestWebRTCServiceAddICECandidate(t *testing.T) {
	service := newTestWebRTCService(t, NewStreamCoordinator(nil, testManagedSourceFactory()), []config.Entrypoint{{ID: "main", Capabilities: config.Capabilities{Stream: true}}})

	offer, client := testWebRTCOffer(t)
	defer client.Close()

	if _, err := service.Offer(context.Background(), "session-1", "main", offer, nil, nil); err != nil {
		t.Fatal(err)
	}
	defer service.Close("session-1")

	if err := service.AddICECandidate("session-1", ICECandidate{Candidate: "candidate:1 1 udp 2130706431 192.0.2.1 12345 typ host"}); err != nil {
		t.Fatalf("AddICECandidate() error = %v", err)
	}

	if err := service.AddICECandidate("", ICECandidate{Candidate: "candidate"}); !errors.Is(err, ErrSessionIDRequired) {
		t.Fatalf("empty session error = %v", err)
	}

	if err := service.AddICECandidate("session-1", ICECandidate{}); !errors.Is(err, ErrCandidateRequired) {
		t.Fatalf("empty candidate error = %v", err)
	}

	if err := service.AddICECandidate("future", ICECandidate{Candidate: "candidate"}); err != nil {
		t.Fatalf("pre-offer candidate error = %v", err)
	}

	if got := len(service.pendingCandidates["future"].candidates); got != 1 {
		t.Fatalf("pre-offer candidates = %d, want 1", got)
	}
}

func TestWebRTCServiceAddICECandidateBoundsPreOfferQueue(t *testing.T) {
	service := newTestWebRTCService(t, NewStreamCoordinator(nil, testManagedSourceFactory()), nil)
	for range maxPendingSessionCandidates + 1 {
		if err := service.AddICECandidate("future", ICECandidate{Candidate: "candidate"}); err != nil {
			t.Fatal(err)
		}
	}

	if got := len(service.pendingCandidates["future"].candidates); got != maxPendingSessionCandidates {
		t.Fatalf("pre-offer candidates = %d, want %d", got, maxPendingSessionCandidates)
	}
}

func TestNormalizeBackchannelPayloadType(t *testing.T) {
	packet := &rtp.Packet{Header: rtp.Header{PayloadType: 111}}
	normalizeBackchannelPayloadType(packet)

	if packet.PayloadType != audioBridgeBackchannelOpusPT {
		t.Fatalf("payload type = %d, want %d", packet.PayloadType, audioBridgeBackchannelOpusPT)
	}

	normalizeBackchannelPayloadType(nil)
}

func TestWebRTCSessionTracksOutboundRTP(t *testing.T) {
	session := &webRTCSession{}
	video := &rtp.Packet{Payload: []byte{1, 2, 3}}
	audio := &rtp.Packet{Payload: []byte{4, 5}}

	if !session.recordOutboundRTP("video", video) {
		t.Fatal("first video packet was not reported")
	}

	if session.recordOutboundRTP("video", video) {
		t.Fatal("subsequent video packet was reported as first")
	}

	if !session.recordOutboundRTP("audio", audio) {
		t.Fatal("first audio packet was not reported")
	}

	if !session.recordOutboundRTPWriteError("audio") {
		t.Fatal("first audio write error was not reported")
	}

	if session.recordOutboundRTPWriteError("audio") {
		t.Fatal("subsequent audio write error was reported as first")
	}

	if got := session.outboundRTPStats("video"); got != (outboundRTPStatsSnapshot{packets: 2, payloadBytes: 6}) {
		t.Fatalf("video stats = %#v", got)
	}

	if got := session.outboundRTPStats("audio"); got != (outboundRTPStatsSnapshot{packets: 1, payloadBytes: 2, writeErrors: 2}) {
		t.Fatalf("audio stats = %#v", got)
	}

	if session.recordOutboundRTP("unknown", video) {
		t.Fatal("unknown track was recorded")
	}
}

func TestWebRTCSessionTracksRTCPFeedback(t *testing.T) {
	session := &webRTCSession{}
	session.recordRTCP("audio", &rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{FractionLost: 4, TotalLost: 12, LastSequenceNumber: 34}}})
	session.recordRTCP("video", &rtcp.TransportLayerNack{})
	session.recordRTCP("video", &rtcp.PictureLossIndication{})
	session.recordRTCP("unknown", &rtcp.PictureLossIndication{})

	if got := session.outboundRTPStats("audio"); got != (outboundRTPStatsSnapshot{receiverReports: 1, reportedFractionLost: 4, reportedTotalLost: 12, reportedLastSequence: 34}) {
		t.Fatalf("audio RTCP stats = %#v", got)
	}

	if got := session.outboundRTPStats("video"); got != (outboundRTPStatsSnapshot{nackFeedback: 1, pliFeedback: 1}) {
		t.Fatalf("video RTCP stats = %#v", got)
	}
}

func newTestWebRTCService(t *testing.T, coordinator *StreamCoordinator, entrypoints []config.Entrypoint) *WebRTCService {
	t.Helper()

	service, err := NewWebRTCService(coordinator, entrypoints)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = service.Shutdown() })

	return service
}

func testWebRTCOffer(t *testing.T) (string, *webrtc.PeerConnection) {
	t.Helper()

	engine := &webrtc.MediaEngine{}
	if err := engine.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}

	client, err := webrtc.NewAPI(webrtc.WithMediaEngine(engine)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}

	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
		t.Fatal(err)
	}

	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}

	gathered := webrtc.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}

	<-gathered

	if client.LocalDescription() == nil || strings.TrimSpace(client.LocalDescription().SDP) == "" {
		t.Fatal("client offer SDP is empty")
	}

	return client.LocalDescription().SDP, client
}

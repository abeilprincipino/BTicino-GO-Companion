package media

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"net"
	"strings"
	"testing"

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
	if port := service.iceConn.LocalAddr().(*net.UDPAddr).Port; port != webrtcICEPort {
		t.Fatalf("ICE UDP port = %d, want %d", port, webrtcICEPort)
	}
	offer, client := testWebRTCOffer(t)
	defer client.Close() //nolint:errcheck // test cleanup
	if err := service.AddICECandidate("session-1", ICECandidate{Candidate: "candidate:1 1 udp 2130706431 192.0.2.1 12345 typ host"}); err != nil {
		t.Fatalf("AddICECandidate() before offer error = %v", err)
	}

	answer, err := service.Offer(context.Background(), "session-1", "main", offer)
	if err != nil {
		t.Fatalf("Offer() error = %v, offer = %q", err, offer)
	}
	if !strings.Contains(answer, "candidate:") {
		t.Fatalf("answer does not contain gathered ICE candidates: %q", answer)
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

func TestWebRTCServiceOfferReplacesPreviousSessionForEntrypoint(t *testing.T) {
	source := &remoteBYESource{}
	coordinator := NewStreamCoordinator(nil, func(_ config.Entrypoint, events SourceEvents) (ManagedSource, func(), error) {
		source.callback = events.RemoteBYE
		return source, nil, nil
	})
	service := newTestWebRTCService(t, coordinator, []config.Entrypoint{{ID: "main", Capabilities: config.Capabilities{Stream: true}}})
	firstOffer, firstClient := testWebRTCOffer(t)
	defer firstClient.Close() //nolint:errcheck // test cleanup
	if _, err := service.Offer(context.Background(), "session-1", "main", firstOffer); err != nil {
		t.Fatalf("offer first session: %v", err)
	}

	secondOffer, secondClient := testWebRTCOffer(t)
	defer secondClient.Close() //nolint:errcheck // test cleanup
	if _, err := service.Offer(context.Background(), "session-2", "main", secondOffer); err != nil {
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
	_, err := service.Offer(context.Background(), "session-1", "missing", "offer")
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
	defer client.Close() //nolint:errcheck // test cleanup

	if _, err := service.Offer(context.Background(), "session-1", "main", offer); err != nil {
		t.Fatal(err)
	}
	defer service.Close("session-1") //nolint:errcheck // test cleanup

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

func newTestWebRTCService(t *testing.T, coordinator *StreamCoordinator, entrypoints []config.Entrypoint) *WebRTCService {
	t.Helper()
	service, err := NewWebRTCService(coordinator, entrypoints)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.iceConn.Close() })
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

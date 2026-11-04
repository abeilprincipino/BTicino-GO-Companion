package media

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
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

	answer, err := service.Offer(context.Background(), "session-1", "main", offer, nil, &OfferTrickle{Candidate: func(candidate *ICECandidate) {
		if candidate == nil {
			return
		}

		candidates <- *candidate
	}, SourceFailed: func(error) {}})
	if err != nil {
		t.Fatalf("Offer() error = %v, offer = %q", err, offer)
	}

	if strings.TrimSpace(answer) == "" {
		t.Fatal("answer is empty")
	}

	// A trickling client does not wait for gathering: its candidates come through
	// the callback, whether or not some are already in the answer.
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

	service.starts.Wait()

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

func TestWebRTCServiceSecondOfferJoinsTheStream(t *testing.T) {
	source := &callEndingSource{}
	created := 0
	coordinator := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		created++
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
		t.Fatalf("offer second session: %v", err)
	}

	service.mu.Lock()
	_, firstExists := service.sessions["session-1"]
	_, secondExists := service.sessions["session-2"]
	service.mu.Unlock()

	if !firstExists || !secondExists || created != 1 {
		t.Fatalf("first=%t second=%t sources=%d, want both sessions on one source", firstExists, secondExists, created)
	}

	if err := service.Close("session-1"); err != nil {
		t.Fatal(err)
	}

	if source.closes != 0 {
		t.Fatalf("closes after the first close = %d, want 0 — another device still watches", source.closes)
	}

	if err := service.Close("session-2"); err != nil {
		t.Fatal(err)
	}

	if source.closes != 1 || source.endCallCalls != 1 {
		t.Fatalf("after the last close closes=%d endCalls=%d, want 1 and 1", source.closes, source.endCallCalls)
	}
}

func TestWebRTCServiceOfferWithoutTrickleAnswersWithCandidates(t *testing.T) {
	coordinator := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return &callEndingSource{}, nil, nil
	})
	service := newTestWebRTCService(t, coordinator, []config.Entrypoint{{ID: "main", Capabilities: config.Capabilities{Stream: true}}})

	offer, client := testWebRTCOffer(t)
	defer client.Close()

	answer, err := service.Offer(context.Background(), "session-1", "main", offer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(answer, "a=candidate:") {
		t.Fatalf("answer does not contain a local ICE candidate: %q", answer)
	}
}

func TestWebRTCServiceLeaveDoesNotEndTheCall(t *testing.T) {
	source := &callEndingSource{}
	coordinator := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})
	service := newTestWebRTCService(t, coordinator, []config.Entrypoint{{ID: "main", Capabilities: config.Capabilities{Stream: true}}})

	offer, client := testWebRTCOffer(t)
	defer client.Close()

	if _, err := service.Offer(context.Background(), "session-1", "main", offer, nil, nil); err != nil {
		t.Fatal(err)
	}

	if err := service.Leave("session-1"); err != nil {
		t.Fatal(err)
	}

	if err := service.Leave("session-1"); err != nil {
		t.Fatalf("second Leave() error = %v, want idempotent", err)
	}

	if source.closes != 1 || source.endCallCalls != 0 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 0", source.closes, source.endCallCalls)
	}

	if err := service.Leave(" "); !errors.Is(err, ErrSessionIDRequired) {
		t.Fatalf("Leave(blank) error = %v, want ErrSessionIDRequired", err)
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

func TestDescribeCandidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		candidate, candidateType, protocol string
		mdns                               bool
	}{
		{candidate: "candidate:1 1 UDP 2130706431 192.0.2.1 12345 typ host", candidateType: "host", protocol: "udp"},
		{candidate: "candidate:2 1 udp 2130706431 a1b2.local 12345 typ host generation 0", candidateType: "host", protocol: "udp", mdns: true},
		{candidate: "candidate:3 1 udp 1 192.0.2.2 3478 typ relay raddr 0.0.0.0 rport 0", candidateType: "relay", protocol: "udp"},
		{candidate: "candidate:4 1"},
	}

	for _, test := range tests {
		candidateType, protocol, mdns := describeCandidate(test.candidate)
		if candidateType != test.candidateType || protocol != test.protocol || mdns != test.mdns {
			t.Errorf("describeCandidate(%q) = %q, %q, %v", test.candidate, candidateType, protocol, mdns)
		}
	}
}

func TestWaitForGathering(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	close(done)

	if complete, err := waitForGathering(context.Background(), done, time.Hour); !complete || err != nil {
		t.Fatalf("completed gathering = %v, %v; want true, nil", complete, err)
	}

	if complete, err := waitForGathering(context.Background(), make(chan struct{}), time.Millisecond); complete || err != nil {
		t.Fatalf("stalled gathering = %v, %v; want false, nil", complete, err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := waitForGathering(cancelled, make(chan struct{}), time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait error = %v, want context.Canceled", err)
	}
}

// blockedSource holds its startup until the test releases it, as an intercom
// takes seconds to answer the SIP call and start the video.
type blockedSource struct {
	callEndingSource
	events   SourceEvents
	starting chan struct{}
	release  chan struct{}
	err      error
}

func (s *blockedSource) Start(context.Context) error {
	close(s.starting)
	<-s.release
	return s.err
}

func newBlockedService(t *testing.T) (*WebRTCService, *StreamCoordinator, *blockedSource) {
	t.Helper()

	source := &blockedSource{starting: make(chan struct{}), release: make(chan struct{})}
	coordinator := NewStreamCoordinator(nil, func(_ config.Entrypoint, events SourceEvents) (ManagedSource, func(), error) {
		source.events = events
		return source, nil, nil
	})

	return newTestWebRTCService(t, coordinator, []config.Entrypoint{{ID: "main", Capabilities: config.Capabilities{Stream: true}}}), coordinator, source
}

// offerAtOnce makes a trickling offer and fails the test if the answer waits for
// the source.
func offerAtOnce(t *testing.T, service *WebRTCService, sessionID string, trickle *OfferTrickle) (string, *webrtc.PeerConnection) {
	t.Helper()

	offer, client := testWebRTCOffer(t)
	t.Cleanup(func() { _ = client.Close() })

	if trickle == nil {
		trickle = &OfferTrickle{Candidate: func(*ICECandidate) {}, SourceFailed: func(error) {}}
	}

	type result struct {
		answer string
		err    error
	}

	done := make(chan result, 1)

	go func() {
		answer, err := service.Offer(context.Background(), sessionID, "main", offer, nil, trickle)
		done <- result{answer, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Offer() error = %v", got.err)
		}

		return got.answer, client
	case <-time.After(5 * time.Second):
		t.Fatal("the answer of a trickling offer waited for the source")
		return "", nil
	}
}

func sessionLease(service *WebRTCService, sessionID string) *StreamLease {
	service.mu.Lock()
	session := service.sessions[sessionID]
	service.mu.Unlock()

	if session == nil {
		return nil
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	return session.lease
}

func TestWebRTCServiceTricklingAnswerDoesNotWaitForTheSource(t *testing.T) {
	service, coordinator, source := newBlockedService(t)

	answer, _ := offerAtOnce(t, service, "session-1", nil)
	if strings.TrimSpace(answer) == "" {
		t.Fatal("answer is empty")
	}

	if coordinator.Snapshot().Owner != StreamOwnerCompanion {
		t.Fatalf("owner = %q, want the stream reserved by the answer", coordinator.Snapshot().Owner)
	}

	close(source.release)
	service.starts.Wait()

	if sessionLease(service, "session-1") == nil {
		t.Fatal("the session did not get its lease once the source started")
	}

	if err := service.Close("session-1"); err != nil {
		t.Fatal(err)
	}

	if source.closes != 1 || source.endCallCalls != 1 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 1", source.closes, source.endCallCalls)
	}
}

func TestWebRTCServiceOfferWithoutTrickleWaitsForTheSource(t *testing.T) {
	service, _, source := newBlockedService(t)

	offer, client := testWebRTCOffer(t)
	defer client.Close()

	done := make(chan error, 1)

	go func() {
		_, err := service.Offer(context.Background(), "session-1", "main", offer, nil, nil)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Offer() returned before the source started: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(source.release)

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if sessionLease(service, "session-1") == nil {
		t.Fatal("the session has no lease after its answer")
	}
}

func TestWebRTCServiceTricklingOfferOnABusyStreamFailsAtOnce(t *testing.T) {
	service, coordinator, source := newBlockedService(t)
	close(source.release)

	holder, err := coordinator.Acquire(context.Background(), config.Entrypoint{ID: "main"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Release(holder)

	offer, client := testWebRTCOffer(t)
	defer client.Close()

	failed := false
	trickle := &OfferTrickle{Candidate: func(*ICECandidate) {}, SourceFailed: func(error) { failed = true }}

	if _, err := service.Offer(context.Background(), "session-1", "main", offer, nil, trickle); !errors.Is(err, ErrStreamBusy) {
		t.Fatalf("Offer() error = %v, want %v", err, ErrStreamBusy)
	}

	service.starts.Wait()

	if failed {
		t.Fatal("a refused offer was also told as a failure after the answer")
	}

	service.mu.Lock()
	_, exists := service.sessions["session-1"]
	service.mu.Unlock()

	if exists {
		t.Fatal("the refused session is still registered")
	}
}

func TestWebRTCServiceSourceFailureAfterTheAnswerIsTold(t *testing.T) {
	service, coordinator, source := newBlockedService(t)
	source.err = errors.New("sip refused")

	failures := make(chan error, 2)
	trickle := &OfferTrickle{Candidate: func(*ICECandidate) {}, SourceFailed: func(err error) { failures <- err }}

	offerAtOnce(t, service, "session-1", trickle)
	close(source.release)
	service.starts.Wait()

	select {
	case err := <-failures:
		if !errors.Is(err, source.err) {
			t.Fatalf("told error = %v, want %v", err, source.err)
		}
	default:
		t.Fatal("the client was not told that the stream did not start")
	}

	service.mu.Lock()
	_, exists := service.sessions["session-1"]
	service.mu.Unlock()

	if exists {
		t.Fatal("the failed session is still registered")
	}

	if owner := coordinator.Snapshot().Owner; owner != StreamOwnerIdle {
		t.Fatalf("owner = %q, want idle", owner)
	}
}

func TestWebRTCServiceCloseDuringStartupReleasesTheStream(t *testing.T) {
	service, coordinator, source := newBlockedService(t)

	failed := false
	offerAtOnce(t, service, "session-1", &OfferTrickle{Candidate: func(*ICECandidate) {}, SourceFailed: func(error) { failed = true }})

	if err := service.Close("session-1"); err != nil {
		t.Fatal(err)
	}

	close(source.release)
	service.starts.Wait()

	if source.closes != 1 || source.endCallCalls != 1 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 1: the consumer let go", source.closes, source.endCallCalls)
	}

	if failed {
		t.Fatal("a client that closed was told of a failure")
	}

	if owner := coordinator.Snapshot().Owner; owner != StreamOwnerIdle {
		t.Fatalf("owner = %q, want idle", owner)
	}
}

func TestWebRTCServiceLeaveDuringStartupDoesNotEndTheCall(t *testing.T) {
	service, _, source := newBlockedService(t)

	offerAtOnce(t, service, "session-1", nil)

	if err := service.Leave("session-1"); err != nil {
		t.Fatal(err)
	}

	close(source.release)
	service.starts.Wait()

	if source.closes != 1 || source.endCallCalls != 0 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 0: the call was answered elsewhere", source.closes, source.endCallCalls)
	}
}

func TestWebRTCServiceSecondTricklingOfferJoinsAStartingStream(t *testing.T) {
	service, _, source := newBlockedService(t)

	offerAtOnce(t, service, "session-1", nil)
	offerAtOnce(t, service, "session-2", nil)

	close(source.release)
	service.starts.Wait()

	if sessionLease(service, "session-1") == nil || sessionLease(service, "session-2") == nil {
		t.Fatal("both sessions should hold the one stream")
	}

	if err := service.Close("session-1"); err != nil {
		t.Fatal(err)
	}

	if source.closes != 0 {
		t.Fatalf("closes = %d, want 0: another device still watches", source.closes)
	}

	if err := service.Close("session-2"); err != nil {
		t.Fatal(err)
	}

	if source.closes != 1 {
		t.Fatalf("closes = %d, want 1", source.closes)
	}
}

// The peer connection comes up while the intercom is still starting, so the
// frames that arrive before the session has its lease must not be lost.
func TestWebRTCServiceConnectsDuringStartupAndReplaysTheKeyframe(t *testing.T) {
	service, _, source := newBlockedService(t)

	candidates := make(chan ICECandidate, 16)
	answer, client := offerAtOnce(t, service, "session-1", &OfferTrickle{Candidate: func(candidate *ICECandidate) {
		if candidate != nil {
			candidates <- *candidate
		}
	}, SourceFailed: func(error) {}})

	connected := make(chan struct{})

	var connectedOnce sync.Once

	client.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			connectedOnce.Do(func() { close(connected) })
		}
	})

	if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	defer close(stop)

	go func() {
		for {
			select {
			case candidate := <-candidates:
				_ = client.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidate.Candidate, SDPMid: candidate.SDPMid, SDPMLineIndex: candidate.SDPMLineIndex})
			case <-stop:
				return
			}
		}
	}()

	select {
	case <-connected:
	case <-time.After(20 * time.Second):
		t.Fatal("the peer connection did not come up while the source was starting")
	}

	service.mu.Lock()
	session := service.sessions["session-1"]
	service.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for session.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		if time.Now().After(deadline) {
			t.Fatalf("service peer state = %s", session.pc.ConnectionState())
		}

		time.Sleep(10 * time.Millisecond)
	}

	<-source.starting
	source.events.VideoRTP(h264Packet(1, 1000, idrSlice...))

	if got := session.outboundRTPStats("video").packets; got != 1 {
		t.Fatalf("video packets before the lease = %d, want 1", got)
	}

	close(source.release)
	service.starts.Wait()

	deadline = time.Now().Add(5 * time.Second)
	for session.outboundRTPStats("video").packets < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the keyframe was not replayed once the session had its lease")
		}

		time.Sleep(10 * time.Millisecond)
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

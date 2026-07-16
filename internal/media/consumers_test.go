package media

import (
	"errors"
	"testing"

	"github.com/pion/rtp"
)

const (
	testOffer    = "offer"
	testOfferSDP = "offer-sdp"
)

func TestDistributor_RegisterSessionConsumerScopesPackets(t *testing.T) {
	t.Parallel()

	distributor := NewDistributor()
	video := testSource()
	audio := video
	audio.MediaKind = MediaKindAudio
	audio.SSRC = 5678

	if err := distributor.RegisterSource(video); err != nil {
		t.Fatal(err)
	}

	if err := distributor.RegisterSource(audio); err != nil {
		t.Fatal(err)
	}

	received := make(chan Packet, 1)

	if err := distributor.RegisterSessionConsumer(video, "video-session", ConsumerFunc(func(packet Packet) {
		received <- packet
	})); err != nil {
		t.Fatal(err)
	}

	if !distributor.Distribute(video, testRTPPacket(video.SSRC)) {
		t.Fatal("Distribute() = false, want true")
	}

	<-received

	if !distributor.Distribute(audio, testRTPPacket(audio.SSRC)) {
		t.Fatal("Distribute() = false, want true")
	}

	select {
	case packet := <-received:
		t.Fatalf("received packet for wrong source: %#v", packet)
	default:
	}
}

func TestWebRTCService_OfferCandidateAndClose(t *testing.T) {
	t.Parallel()

	distributor := NewDistributor()

	source := testSource()
	if err := distributor.RegisterSource(source); err != nil {
		t.Fatal(err)
	}

	peer := &fakeWebRTCPeer{answer: SessionDescription{Type: "answer", SDP: "answer-sdp"}}
	factory := fakeWebRTCPeerFactory{peer: peer}
	announced := make(chan ICECandidate, 1)
	service := NewWebRTCService(distributor, factory, nil, CandidateSinkFunc(func(_ SessionID, candidate ICECandidate) {
		announced <- candidate
	}))

	answer, err := service.Offer(source, "web-1", SessionDescription{Type: testOffer, SDP: testOfferSDP})
	if err != nil {
		t.Fatal(err)
	}

	if answer != peer.answer {
		t.Fatalf("Offer() answer = %#v, want %#v", answer, peer.answer)
	}

	if peer.remote != (SessionDescription{Type: testOffer, SDP: testOfferSDP}) || peer.local != peer.answer {
		t.Fatalf("peer descriptions = remote %#v local %#v", peer.remote, peer.local)
	}

	candidate := ICECandidate{Candidate: "candidate:1"}
	if err := service.AddCandidate("web-1", candidate); err != nil {
		t.Fatal(err)
	}

	if peer.candidate != candidate {
		t.Fatalf("candidate = %#v, want %#v", peer.candidate, candidate)
	}

	peer.candidates.SendCandidate("web-1", candidate)

	if got := <-announced; got != candidate {
		t.Fatalf("announced candidate = %#v, want %#v", got, candidate)
	}

	if !distributor.Distribute(source, testRTPPacket(source.SSRC)) {
		t.Fatal("Distribute() = false, want true")
	}

	if peer.writer.calls != 1 {
		t.Fatalf("writer calls = %d, want 1", peer.writer.calls)
	}

	if err := service.Close("web-1"); err != nil {
		t.Fatal(err)
	}

	if !peer.closed {
		t.Fatal("peer was not closed")
	}

	if err := service.AddCandidate("web-1", candidate); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("AddCandidate() error = %v, want %v", err, ErrSessionNotFound)
	}

	if err := service.Close("web-1"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Close() error = %v, want %v", err, ErrSessionNotFound)
	}
}

func TestWebRTCService_OfferRejectsDuplicateSession(t *testing.T) {
	t.Parallel()

	distributor := NewDistributor()

	source := testSource()
	if err := distributor.RegisterSource(source); err != nil {
		t.Fatal(err)
	}

	service := NewWebRTCService(distributor, fakeWebRTCPeerFactory{peer: &fakeWebRTCPeer{answer: SessionDescription{Type: "answer", SDP: "answer-sdp"}}}, nil, nil)
	if _, err := service.Offer(source, "web-1", SessionDescription{Type: testOffer, SDP: testOfferSDP}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Offer(source, "web-1", SessionDescription{Type: testOffer, SDP: testOfferSDP}); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("Offer() error = %v, want %v", err, ErrSessionExists)
	}
}

type recordingWriter struct {
	calls int
}

func (w *recordingWriter) WriteRTP(_ *rtp.Packet) error {
	w.calls++
	return nil
}

type fakeWebRTCPeerFactory struct {
	peer *fakeWebRTCPeer
}

func (f fakeWebRTCPeerFactory) NewPeer(_ Source, _ SessionID, _ Backchannel, candidates CandidateSink) (WebRTCPeer, error) {
	f.peer.candidates = candidates
	return f.peer, nil
}

type fakeWebRTCPeer struct {
	answer     SessionDescription
	remote     SessionDescription
	local      SessionDescription
	candidate  ICECandidate
	writer     recordingWriter
	closed     bool
	candidates CandidateSink
}

func (p *fakeWebRTCPeer) AddTrack(Source) (RTPWriter, error) {
	return &p.writer, nil
}

func (p *fakeWebRTCPeer) SetRemoteDescription(description SessionDescription) error {
	p.remote = description
	return nil
}

func (p *fakeWebRTCPeer) CreateAnswer() (SessionDescription, error) {
	return p.answer, nil
}

func (p *fakeWebRTCPeer) SetLocalDescription(description SessionDescription) error {
	p.local = description
	return nil
}

func (p *fakeWebRTCPeer) AddICECandidate(candidate ICECandidate) error {
	p.candidate = candidate
	return nil
}

func (p *fakeWebRTCPeer) Close() error {
	p.closed = true
	return nil
}

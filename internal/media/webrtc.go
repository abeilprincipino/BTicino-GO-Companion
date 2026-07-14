package media

import (
	"errors"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var (
	ErrInvalidOffer    = errors.New("media: invalid WebRTC offer")
	ErrSessionExists   = errors.New("media: WebRTC session already exists")
	ErrSessionNotFound = errors.New("media: WebRTC session not found")
)

type SessionDescription struct {
	Type string
	SDP  string
}

type ICECandidate struct {
	Candidate        string
	SDPMid           *string
	SDPMLineIndex    *uint16
	UsernameFragment *string
}

type CandidateSink interface {
	SendCandidate(SessionID, ICECandidate)
}

type CandidateSinkFunc func(SessionID, ICECandidate)

func (f CandidateSinkFunc) SendCandidate(sessionID SessionID, candidate ICECandidate) {
	f(sessionID, candidate)
}

type WebRTCPeer interface {
	AddTrack(Source) (RTPWriter, error)
	SetRemoteDescription(SessionDescription) error
	CreateAnswer() (SessionDescription, error)
	SetLocalDescription(SessionDescription) error
	AddICECandidate(ICECandidate) error
	Close() error
}

type WebRTCPeerFactory interface {
	NewPeer(Source, SessionID, Backchannel, CandidateSink) (WebRTCPeer, error)
}

type WebRTCService struct {
	mu sync.Mutex

	distributor *Distributor
	factory     WebRTCPeerFactory
	backchannel Backchannel
	candidates  CandidateSink
	sessions    map[SessionID]webRTCSession
}

type webRTCSession struct {
	source Source
	peer   WebRTCPeer
}

func NewWebRTCService(distributor *Distributor, factory WebRTCPeerFactory, backchannel Backchannel, candidates CandidateSink) *WebRTCService {
	return &WebRTCService{
		distributor: distributor,
		factory:     factory,
		backchannel: backchannel,
		candidates:  candidates,
		sessions:    map[SessionID]webRTCSession{},
	}
}

func (s *WebRTCService) Offer(source Source, sessionID SessionID, offer SessionDescription) (SessionDescription, error) {
	if sessionID == "" || offer.Type != "offer" || offer.SDP == "" || s.distributor == nil || s.factory == nil {
		return SessionDescription{}, ErrInvalidOffer
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sessionID]; ok {
		return SessionDescription{}, ErrSessionExists
	}

	peer, err := s.factory.NewPeer(source, sessionID, s.backchannel, s.candidates)
	if err != nil {
		return SessionDescription{}, err
	}
	writer, err := peer.AddTrack(source)
	if err != nil {
		_ = peer.Close()
		return SessionDescription{}, err
	}
	if err := peer.SetRemoteDescription(offer); err != nil {
		_ = peer.Close()
		return SessionDescription{}, err
	}
	answer, err := peer.CreateAnswer()
	if err != nil {
		_ = peer.Close()
		return SessionDescription{}, err
	}
	if err := peer.SetLocalDescription(answer); err != nil {
		_ = peer.Close()
		return SessionDescription{}, err
	}
	if err := s.distributor.RegisterSessionConsumer(source, sessionID, ConsumerFunc(func(packet Packet) {
		_ = writer.WriteRTP(packet.RTP)
	})); err != nil {
		_ = peer.Close()
		return SessionDescription{}, err
	}
	s.sessions[sessionID] = webRTCSession{source: source, peer: peer}
	return answer, nil
}

func (s *WebRTCService) AddCandidate(sessionID SessionID, candidate ICECandidate) error {
	s.mu.Lock()
	session, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return ErrSessionNotFound
	}
	return session.peer.AddICECandidate(candidate)
}

func (s *WebRTCService) Close(sessionID SessionID) error {
	s.mu.Lock()
	session, ok := s.sessions[sessionID]
	if ok {
		delete(s.sessions, sessionID)
	}
	s.mu.Unlock()
	if !ok {
		return ErrSessionNotFound
	}
	s.distributor.UnregisterSessionConsumer(session.source, sessionID)
	return session.peer.Close()
}

type PionPeerFactory struct {
	Configuration webrtc.Configuration
}

func (f PionPeerFactory) NewPeer(_ Source, sessionID SessionID, backchannel Backchannel, candidates CandidateSink) (WebRTCPeer, error) {
	peer, err := webrtc.NewPeerConnection(f.Configuration)
	if err != nil {
		return nil, err
	}
	p := &pionPeer{peer: peer, backchannel: backchannel}
	peer.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil || candidates == nil {
			return
		}
		value := candidate.ToJSON()
		candidates.SendCandidate(sessionID, ICECandidate{
			Candidate:        value.Candidate,
			SDPMid:           value.SDPMid,
			SDPMLineIndex:    value.SDPMLineIndex,
			UsernameFragment: value.UsernameFragment,
		})
	})
	peer.OnTrack(p.onTrack)
	return p, nil
}

type pionPeer struct {
	peer        *webrtc.PeerConnection
	backchannel Backchannel
}

func (p *pionPeer) AddTrack(source Source) (RTPWriter, error) {
	capability := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}
	trackID := "video"
	if source.MediaKind == MediaKindAudio {
		capability = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
		trackID = "audio"
	}
	track, err := webrtc.NewTrackLocalStaticRTP(capability, trackID, string(source.EntrypointID))
	if err != nil {
		return nil, err
	}
	if _, err := p.peer.AddTrack(track); err != nil {
		return nil, err
	}
	return pionTrackWriter{track: track}, nil
}

func (p *pionPeer) SetRemoteDescription(description SessionDescription) error {
	return p.peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: description.SDP})
}

func (p *pionPeer) CreateAnswer() (SessionDescription, error) {
	answer, err := p.peer.CreateAnswer(nil)
	if err != nil {
		return SessionDescription{}, err
	}
	return SessionDescription{Type: answer.Type.String(), SDP: answer.SDP}, nil
}

func (p *pionPeer) SetLocalDescription(description SessionDescription) error {
	return p.peer.SetLocalDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: description.SDP})
}

func (p *pionPeer) AddICECandidate(candidate ICECandidate) error {
	return p.peer.AddICECandidate(webrtc.ICECandidateInit{
		Candidate:        candidate.Candidate,
		SDPMid:           candidate.SDPMid,
		SDPMLineIndex:    candidate.SDPMLineIndex,
		UsernameFragment: candidate.UsernameFragment,
	})
}

func (p *pionPeer) Close() error {
	return p.peer.Close()
}

func (p *pionPeer) onTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	if p.backchannel == nil || track.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if p.backchannel.WriteRTP(packet) != nil {
			return
		}
	}
}

type pionTrackWriter struct {
	track *webrtc.TrackLocalStaticRTP
}

func (w pionTrackWriter) WriteRTP(packet *rtp.Packet) error {
	if packet == nil {
		return ErrInvalidBackchannelPacket
	}
	return w.track.WriteRTP(packet)
}

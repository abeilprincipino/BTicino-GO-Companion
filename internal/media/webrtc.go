package media

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/system"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	pendingCandidateTTL         = 45 * time.Second
	maxPendingSessionCandidates = 64
	webrtcICEPort               = 8555
)

var (
	ErrSessionIDRequired  = errors.New("media: session_id is required")
	ErrEntrypointRequired = errors.New("media: entrypoint_id is required")
	ErrOfferRequired      = errors.New("media: offer_sdp is required")
	ErrCandidateRequired  = errors.New("media: candidate is required")
	ErrSessionExists      = errors.New("media: WebRTC session already exists")
	ErrSessionNotFound    = errors.New("media: WebRTC session not found")
	ErrEntrypointNotFound = errors.New("media: WebRTC entrypoint not found")
)

// ICECandidate is a remote ICE candidate supplied by a WebRTC client.
type ICECandidate struct {
	Candidate        string  `json:"candidate"`
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

// WebRTCService serves one WebRTC peer from the same exclusive source lease
// used by RTSP. The intercom permits only one active source at a time.
type WebRTCService struct {
	mu                sync.Mutex
	offerMu           sync.Mutex
	coordinator       *StreamCoordinator
	entrypoints       map[string]config.Entrypoint
	api               *webrtc.API
	configuration     webrtc.Configuration
	iceConn           net.PacketConn
	sessions          map[string]*webRTCSession
	pendingCandidates map[string]pendingCandidateBatch
}

type webRTCSession struct {
	mu                   sync.Mutex
	id                   string
	entrypointID         string
	pc                   *webrtc.PeerConnection
	lease                *StreamLease
	videoTrack           *webrtc.TrackLocalStaticRTP
	audioTrack           *webrtc.TrackLocalStaticRTP
	pendingRemoteICE     []webrtc.ICECandidateInit
	remoteDescriptionSet bool
	closeOnce            sync.Once
}

type pendingCandidateBatch struct {
	candidates []webrtc.ICECandidateInit
	updatedAt  time.Time
}

func NewWebRTCService(coordinator *StreamCoordinator, entrypoints []config.Entrypoint) (*WebRTCService, error) {
	iceConn, err := net.ListenPacket("udp4", fmt.Sprintf(":%d", webrtcICEPort))
	if err != nil {
		return nil, fmt.Errorf("listen WebRTC ICE UDP %d: %w", webrtcICEPort, err)
	}
	interface_, err := system.PreferredOutboundInterface()
	if err != nil {
		_ = iceConn.Close()
		return nil, fmt.Errorf("select WebRTC interface: %w", err)
	}

	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetICEUDPMux(ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: iceConn}))
	settingEngine.SetInterfaceFilter(func(name string) bool {
		return name == interface_.Name
	})
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine))

	configured := make(map[string]config.Entrypoint, len(entrypoints))
	for _, entrypoint := range entrypoints {
		if entrypoint.Capabilities.Stream && strings.TrimSpace(entrypoint.ID) != "" {
			configured[entrypoint.ID] = entrypoint
		}
	}
	return &WebRTCService{
		coordinator:       coordinator,
		api:               api,
		entrypoints:       configured,
		sessions:          make(map[string]*webRTCSession),
		pendingCandidates: make(map[string]pendingCandidateBatch),
		iceConn:           iceConn,
	}, nil
}

// Offer creates a non-trickle answer. The returned SDP contains all gathered
// local candidates, so clients do not need a candidate endpoint.
func (s *WebRTCService) Offer(ctx context.Context, sessionID, entrypointID, offerSDP string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	entrypointID = strings.TrimSpace(entrypointID)
	offerSDP = canonicalizeSDP(offerSDP)
	if sessionID == "" {
		return "", ErrSessionIDRequired
	}
	if entrypointID == "" {
		return "", ErrEntrypointRequired
	}
	if offerSDP == "" {
		return "", ErrOfferRequired
	}
	if s.coordinator == nil {
		return "", ErrStreamBusy
	}
	s.offerMu.Lock()
	defer s.offerMu.Unlock()

	s.mu.Lock()
	s.prunePendingCandidatesLocked(time.Now())
	entrypoint, found := s.entrypoints[entrypointID]
	if !found {
		s.mu.Unlock()
		return "", ErrEntrypointNotFound
	}
	if _, exists := s.sessions[sessionID]; exists {
		s.mu.Unlock()
		return "", ErrSessionExists
	}
	// HA may create a successor player before its prior player has finished
	// closing. One intercom source cannot serve both independent leases.
	previousSessionIDs := make([]string, 0, len(s.sessions))
	for id, session := range s.sessions {
		if session.entrypointID == entrypointID {
			previousSessionIDs = append(previousSessionIDs, id)
		}
	}
	s.mu.Unlock()
	for _, previousSessionID := range previousSessionIDs {
		s.closeSession(previousSessionID)
	}

	pc, err := s.api.NewPeerConnection(s.configuration)
	if err != nil {
		return "", err
	}
	session := &webRTCSession{id: sessionID, entrypointID: entrypointID, pc: pc}
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			go s.closeSession(sessionID)
		}
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track == nil || track.Kind() != webrtc.RTPCodecTypeAudio || !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeOpus) {
			return
		}
		go s.forwardBackchannel(session, track)
	})

	if err := s.addOutboundTracks(session, entrypoint.ID); err != nil {
		_ = pc.Close()
		return "", err
	}

	s.mu.Lock()
	if _, exists := s.sessions[sessionID]; exists {
		s.mu.Unlock()
		_ = pc.Close()
		return "", ErrSessionExists
	}
	if pending, ok := s.pendingCandidates[sessionID]; ok {
		session.pendingRemoteICE = append(session.pendingRemoteICE, pending.candidates...)
		delete(s.pendingCandidates, sessionID)
	}
	s.sessions[sessionID] = session
	s.mu.Unlock()

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		s.closeSession(sessionID)
		return "", err
	}
	if err := s.flushPendingRemoteCandidates(session); err != nil {
		s.closeSession(sessionID)
		return "", err
	}

	// The HTTP request ends after this answer; the source must instead live
	// until the peer or client explicitly closes the session.
	lease, err := s.coordinator.Acquire(context.WithoutCancel(ctx), entrypoint, SourceEvents{
		VideoRTP:  func(packet *rtp.Packet) { s.writeRTP(session.videoTrack, packet) },
		AudioRTP:  func(packet *rtp.Packet) { s.writeRTP(session.audioTrack, packet) },
		RemoteBYE: func() { s.closeSession(sessionID) },
		Failed:    func(error) { s.closeSession(sessionID) },
	})
	if err != nil {
		s.closeSession(sessionID)
		return "", err
	}
	if !session.setLease(lease) {
		s.coordinator.Release(lease)
		return "", context.Canceled
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.closeSession(sessionID)
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		s.closeSession(sessionID)
		return "", err
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		s.closeSession(sessionID)
		return "", ctx.Err()
	}
	local := pc.LocalDescription()
	if local == nil || strings.TrimSpace(local.SDP) == "" {
		s.closeSession(sessionID)
		return "", errors.New("media: local WebRTC answer is unavailable")
	}
	return local.SDP, nil
}

func (s *WebRTCService) addOutboundTracks(session *webRTCSession, streamID string) error {
	video, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
	}, "video", streamID)
	if err != nil {
		return err
	}
	audio, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
		SDPFmtpLine: "minptime=10;useinbandfec=0;stereo=0;sprop-stereo=0",
	}, "audio", streamID)
	if err != nil {
		return err
	}
	videoSender, err := session.pc.AddTransceiverFromTrack(video, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		return err
	}
	audioSender, err := session.pc.AddTransceiverFromTrack(audio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	if err != nil {
		return err
	}
	go drainRTCP(videoSender.Sender())
	go drainRTCP(audioSender.Sender())
	session.videoTrack, session.audioTrack = video, audio
	return nil
}

func (s *WebRTCService) writeRTP(track *webrtc.TrackLocalStaticRTP, packet *rtp.Packet) {
	if track != nil && packet != nil {
		_ = track.WriteRTP(packet)
	}
}

func (s *WebRTCService) forwardBackchannel(session *webRTCSession, track *webrtc.TrackRemote) {
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		session.mu.Lock()
		lease := session.lease
		session.mu.Unlock()
		if lease != nil {
			normalizeBackchannelPayloadType(packet)
			_ = s.coordinator.WriteBackchannelRTP(lease, packet)
		}
	}
}

func normalizeBackchannelPayloadType(packet *rtp.Packet) {
	if packet != nil {
		packet.PayloadType = audioBridgeBackchannelOpusPT
	}
}

// Close is idempotent so clients can safely retry teardown after a network loss.
func (s *WebRTCService) Close(sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return ErrSessionIDRequired
	}
	s.closeSession(sessionID)
	return nil
}

// AddICECandidate adds a trickled remote candidate to an active session.
// Offer deliberately remains non-trickle and continues returning a fully
// gathered answer for existing clients.
func (s *WebRTCService) AddICECandidate(sessionID string, candidate ICECandidate) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ErrSessionIDRequired
	}
	if strings.TrimSpace(candidate.Candidate) == "" {
		return ErrCandidateRequired
	}

	init := webrtc.ICECandidateInit{
		Candidate:        candidate.Candidate,
		SDPMid:           candidate.SDPMid,
		SDPMLineIndex:    candidate.SDPMLineIndex,
		UsernameFragment: candidate.UsernameFragment,
	}

	s.mu.Lock()
	s.prunePendingCandidatesLocked(time.Now())
	session := s.sessions[sessionID]
	if session == nil {
		batch := s.pendingCandidates[sessionID]
		if len(batch.candidates) < maxPendingSessionCandidates {
			batch.candidates = append(batch.candidates, init)
		}
		batch.updatedAt = time.Now()
		s.pendingCandidates[sessionID] = batch
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	return s.addCandidateToSession(session, init)
}

func (s *WebRTCService) addCandidateToSession(session *webRTCSession, candidate webrtc.ICECandidateInit) error {
	session.mu.Lock()
	if !session.remoteDescriptionSet {
		if len(session.pendingRemoteICE) < maxPendingSessionCandidates {
			session.pendingRemoteICE = append(session.pendingRemoteICE, candidate)
		}
		session.mu.Unlock()
		return nil
	}
	pc := session.pc
	session.mu.Unlock()
	return pc.AddICECandidate(candidate)
}

func (s *WebRTCService) flushPendingRemoteCandidates(session *webRTCSession) error {
	session.mu.Lock()
	session.remoteDescriptionSet = true
	pending := append([]webrtc.ICECandidateInit(nil), session.pendingRemoteICE...)
	session.pendingRemoteICE = nil
	pc := session.pc
	session.mu.Unlock()

	for _, candidate := range pending {
		if err := pc.AddICECandidate(candidate); err != nil {
			return err
		}
	}
	return nil
}

func (s *WebRTCService) closeSession(sessionID string) {
	s.mu.Lock()
	session := s.sessions[sessionID]
	if session != nil {
		delete(s.sessions, sessionID)
	}
	delete(s.pendingCandidates, sessionID)
	s.mu.Unlock()
	if session == nil {
		return
	}
	session.closeOnce.Do(func() {
		session.mu.Lock()
		lease := session.lease
		session.lease = nil
		session.mu.Unlock()
		if lease != nil {
			s.coordinator.Release(lease)
		}
		_ = session.pc.Close()
	})
}

func (s *WebRTCService) prunePendingCandidatesLocked(now time.Time) {
	for sessionID, batch := range s.pendingCandidates {
		if now.Sub(batch.updatedAt) > pendingCandidateTTL {
			delete(s.pendingCandidates, sessionID)
		}
	}
}

func (s *webRTCSession) setLease(lease *StreamLease) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return false
	}
	s.lease = lease
	return true
}

func canonicalizeSDP(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.ReplaceAll(value, "\n", "\r\n") + "\r\n"
}

func drainRTCP(sender *webrtc.RTPSender) {
	if sender == nil {
		return
	}
	buffer := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buffer); err != nil {
			return
		}
	}
}

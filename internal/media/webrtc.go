package media

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/system"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtcp"
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
	logger            *slog.Logger
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
	createdAt            time.Time
	queuedCandidates     atomic.Uint64
	appliedCandidates    atomic.Uint64
	videoRTP             outboundRTPStats
	audioRTP             outboundRTPStats
	closeOnce            sync.Once
}

type outboundRTPStats struct {
	packets              atomic.Uint64
	payloadBytes         atomic.Uint64
	writeErrors          atomic.Uint64
	receiverReports      atomic.Uint64
	reportedFractionLost atomic.Uint64
	reportedTotalLost    atomic.Uint64
	reportedLastSequence atomic.Uint64
	nackFeedback         atomic.Uint64
	pliFeedback          atomic.Uint64
	firstPacket          atomic.Bool
	firstWriteError      atomic.Bool
}

type outboundRTPStatsSnapshot struct {
	packets              uint64
	payloadBytes         uint64
	writeErrors          uint64
	receiverReports      uint64
	reportedFractionLost uint64
	reportedTotalLost    uint64
	reportedLastSequence uint64
	nackFeedback         uint64
	pliFeedback          uint64
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
		logger:            slog.Default().With("component", "media.webrtc"),
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
		s.closeSession(previousSessionID, "superseded by successor session")
	}

	pc, err := s.api.NewPeerConnection(s.configuration)
	if err != nil {
		return "", err
	}
	session := &webRTCSession{id: sessionID, entrypointID: entrypointID, pc: pc, createdAt: time.Now()}
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		s.logger.Debug("webrtc peer connection state changed", "session_id", sessionID, "entrypoint_id", entrypointID, "state", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			go s.closeSession(sessionID, "peer connection "+state.String())
		}
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		s.logger.Debug("webrtc ICE connection state changed", "session_id", sessionID, "entrypoint_id", entrypointID, "state", state.String())
		if state == webrtc.ICEConnectionStateConnected {
			s.logger.Info("webrtc ICE connected", "session_id", sessionID, "entrypoint_id", entrypointID)
		}
	})
	pc.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		s.logger.Debug("webrtc ICE gathering state changed", "session_id", sessionID, "entrypoint_id", entrypointID, "state", state.String())
	})
	pc.OnSignalingStateChange(func(state webrtc.SignalingState) {
		s.logger.Debug("webrtc signaling state changed", "session_id", sessionID, "entrypoint_id", entrypointID, "state", state.String())
	})
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track == nil || track.Kind() != webrtc.RTPCodecTypeAudio || !strings.EqualFold(track.Codec().MimeType, webrtc.MimeTypeOpus) {
			return
		}
		s.logger.Debug("webrtc backchannel track received", "session_id", sessionID, "entrypoint_id", entrypointID, "codec", track.Codec().MimeType, "payload_type", track.PayloadType(), "ssrc", track.SSRC())
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
		session.queuedCandidates.Add(uint64(len(pending.candidates)))
		delete(s.pendingCandidates, sessionID)
	}
	s.sessions[sessionID] = session
	s.mu.Unlock()

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		s.closeSession(sessionID, "set remote description failed")
		return "", err
	}
	if err := s.flushPendingRemoteCandidates(session); err != nil {
		s.closeSession(sessionID, "apply remote candidates failed")
		return "", err
	}

	// The signaling request ends after this answer; the source must instead live
	// until the peer or client explicitly closes the session.
	lease, err := s.coordinator.Acquire(context.WithoutCancel(ctx), entrypoint, SourceEvents{
		VideoRTP:  func(packet *rtp.Packet) { s.writeRTP(session, "video", session.videoTrack, packet) },
		AudioRTP:  func(packet *rtp.Packet) { s.writeRTP(session, "audio", session.audioTrack, packet) },
		RemoteBYE: func() { s.closeSession(sessionID, "remote SIP BYE") },
		Failed:    func(error) { s.closeSession(sessionID, "source failure") },
	})
	if err != nil {
		s.closeSession(sessionID, "source startup failed")
		return "", err
	}
	if !session.setLease(lease) {
		s.coordinator.Release(lease)
		return "", context.Canceled
	}
	s.logger.DebugContext(ctx, "webrtc source started", "session_id", sessionID, "entrypoint_id", entrypointID)

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.closeSession(sessionID, "create answer failed")
		return "", err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		s.closeSession(sessionID, "set local description failed")
		return "", err
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		s.closeSession(sessionID, "answer gathering canceled")
		return "", ctx.Err()
	}
	local := pc.LocalDescription()
	if local == nil || strings.TrimSpace(local.SDP) == "" {
		s.closeSession(sessionID, "local answer unavailable")
		return "", errors.New("media: local WebRTC answer is unavailable")
	}
	s.logNegotiatedMedia(ctx, session)
	s.logger.DebugContext(ctx, "webrtc offer accepted", "session_id", sessionID, "entrypoint_id", entrypointID, "replaced_sessions", len(previousSessionIDs), "remote_candidates_queued", session.queuedCandidates.Load(), "remote_candidates_applied", session.appliedCandidates.Load())
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
	go s.drainRTCP(session, "video", videoSender.Sender())
	go s.drainRTCP(session, "audio", audioSender.Sender())
	session.videoTrack, session.audioTrack = video, audio
	return nil
}

func (s *WebRTCService) logNegotiatedMedia(ctx context.Context, session *webRTCSession) {
	for _, transceiver := range session.pc.GetTransceivers() {
		sender := transceiver.Sender()
		if sender == nil || sender.Track() == nil {
			continue
		}
		parameters := sender.GetParameters()
		codecs := make([]string, 0, len(parameters.Codecs))
		for _, codec := range parameters.Codecs {
			codecs = append(codecs, fmt.Sprintf("%s/%d", codec.MimeType, codec.PayloadType))
		}
		s.logger.DebugContext(ctx, "webrtc negotiated media", "session_id", session.id, "entrypoint_id", session.entrypointID, "kind", transceiver.Kind().String(), "direction", transceiver.Direction().String(), "sender_has_track", true, "encoding_count", len(parameters.Encodings), "codecs", codecs)
	}
}

func (s *WebRTCService) writeRTP(session *webRTCSession, trackName string, track *webrtc.TrackLocalStaticRTP, packet *rtp.Packet) {
	if session == nil || track == nil || packet == nil {
		return
	}
	if err := track.WriteRTP(packet); err != nil {
		if session.recordOutboundRTPWriteError(trackName) {
			s.logger.Debug("webrtc outbound RTP write failed", "session_id", session.id, "entrypoint_id", session.entrypointID, "track", trackName, "error", err)
		}
		return
	}
	if session.recordOutboundRTP(trackName, packet) {
		s.logger.Info("webrtc first RTP packet", "session_id", session.id, "entrypoint_id", session.entrypointID, "track", trackName)
	}
}

func (s *WebRTCService) drainRTCP(session *webRTCSession, trackName string, sender *webrtc.RTPSender) {
	if session == nil || sender == nil {
		return
	}
	for {
		packets, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, packet := range packets {
			session.recordRTCP(trackName, packet)
		}
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
	s.closeSession(sessionID, "client requested close")
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
			session.queuedCandidates.Add(1)
		}
		session.mu.Unlock()
		return nil
	}
	pc := session.pc
	session.mu.Unlock()
	if err := pc.AddICECandidate(candidate); err != nil {
		return err
	}
	session.appliedCandidates.Add(1)
	return nil
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
		session.appliedCandidates.Add(1)
	}
	return nil
}

func (s *WebRTCService) closeSession(sessionID, reason string) {
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
		video := session.outboundRTPStats("video")
		audio := session.outboundRTPStats("audio")
		s.logger.Info("webrtc session closed", "session_id", session.id, "entrypoint_id", session.entrypointID, "reason", reason, "duration", time.Since(session.createdAt).Round(time.Millisecond))
		s.logger.Debug("webrtc session statistics", "session_id", session.id, "entrypoint_id", session.entrypointID, "peer_connection_state", session.pc.ConnectionState().String(), "ice_connection_state", session.pc.ICEConnectionState().String(), "remote_candidates_queued", session.queuedCandidates.Load(), "remote_candidates_applied", session.appliedCandidates.Load(), "video_rtp_packets", video.packets, "video_rtp_payload_bytes", video.payloadBytes, "video_rtp_write_errors", video.writeErrors, "video_rtcp_receiver_reports", video.receiverReports, "video_rtcp_reported_fraction_lost", video.reportedFractionLost, "video_rtcp_reported_total_lost", video.reportedTotalLost, "video_rtcp_reported_last_sequence", video.reportedLastSequence, "video_rtcp_nack_feedback", video.nackFeedback, "video_rtcp_pli_feedback", video.pliFeedback, "audio_rtp_packets", audio.packets, "audio_rtp_payload_bytes", audio.payloadBytes, "audio_rtp_write_errors", audio.writeErrors, "audio_rtcp_receiver_reports", audio.receiverReports, "audio_rtcp_reported_fraction_lost", audio.reportedFractionLost, "audio_rtcp_reported_total_lost", audio.reportedTotalLost, "audio_rtcp_reported_last_sequence", audio.reportedLastSequence, "audio_rtcp_nack_feedback", audio.nackFeedback, "audio_rtcp_pli_feedback", audio.pliFeedback)
		_ = session.pc.Close()
	})
}

func (s *webRTCSession) recordOutboundRTP(trackName string, packet *rtp.Packet) bool {
	stats := s.outboundStats(trackName)
	if stats == nil {
		return false
	}
	stats.packets.Add(1)
	stats.payloadBytes.Add(uint64(len(packet.Payload)))
	return stats.firstPacket.CompareAndSwap(false, true)
}

func (s *webRTCSession) recordOutboundRTPWriteError(trackName string) bool {
	stats := s.outboundStats(trackName)
	if stats == nil {
		return false
	}
	stats.writeErrors.Add(1)
	return stats.firstWriteError.CompareAndSwap(false, true)
}

func (s *webRTCSession) recordRTCP(trackName string, packet rtcp.Packet) {
	stats := s.outboundStats(trackName)
	if stats == nil || packet == nil {
		return
	}
	switch feedback := packet.(type) {
	case *rtcp.ReceiverReport:
		for _, report := range feedback.Reports {
			stats.receiverReports.Add(1)
			stats.reportedFractionLost.Store(uint64(report.FractionLost))
			stats.reportedTotalLost.Store(uint64(report.TotalLost))
			stats.reportedLastSequence.Store(uint64(report.LastSequenceNumber))
		}
	case *rtcp.TransportLayerNack:
		stats.nackFeedback.Add(1)
	case *rtcp.PictureLossIndication:
		stats.pliFeedback.Add(1)
	}
}

func (s *webRTCSession) outboundRTPStats(trackName string) outboundRTPStatsSnapshot {
	stats := s.outboundStats(trackName)
	if stats == nil {
		return outboundRTPStatsSnapshot{}
	}
	return outboundRTPStatsSnapshot{packets: stats.packets.Load(), payloadBytes: stats.payloadBytes.Load(), writeErrors: stats.writeErrors.Load(), receiverReports: stats.receiverReports.Load(), reportedFractionLost: stats.reportedFractionLost.Load(), reportedTotalLost: stats.reportedTotalLost.Load(), reportedLastSequence: stats.reportedLastSequence.Load(), nackFeedback: stats.nackFeedback.Load(), pliFeedback: stats.pliFeedback.Load()}
}

func (s *webRTCSession) outboundStats(trackName string) *outboundRTPStats {
	switch trackName {
	case "video":
		return &s.videoRTP
	case "audio":
		return &s.audioRTP
	default:
		return nil
	}
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

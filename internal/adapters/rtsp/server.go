package rtspadapter

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
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

const (
	streamAutostartTimeout = 12 * time.Second
	streamAutostopTimeout  = 6 * time.Second
	readerWatchdogInterval = 10 * time.Second
	readerIdleTimeout      = 40 * time.Second
	btReturnAudioAddr      = "127.0.0.1:4000"
	snapshotWarmupFrames   = 5

	rtpPayloadTypeH264             = 96
	rtpPayloadTypeSpeex            = 110
	rtpPayloadTypeSpeexBackchannel = 97
	rtpSpeexSampleRate             = 8000

	h264NALTypeIDR   = 5
	h264NALTypeSPS   = 7
	h264NALTypePPS   = 8
	h264NALTypeSTAPA = 24
	h264NALTypeFUA   = 28
)

var ErrSnapshotMirrorBusy = errors.New("rtsp snapshot mirror already active")

type readerInfo struct {
	SessionID    string
	EntrypointID string
	DevAddr      string
	LastSeen     time.Time
}

type Server struct {
	cfg    config.Config
	logger *log.Logger

	transport *Transport

	srv *gortsplib.Server

	mu       sync.RWMutex
	stream   *gortsplib.ServerStream
	videoMed *description.Media
	audioMed *description.Media
	backMed  *description.Media
	readers  map[*gortsplib.ServerSession]readerInfo
	paths    map[string]entrypoint.StreamRoute
	pathList []string

	returnAudio *returnAudioForwarder
	audioBridge *audiobridge.Service
	bridgeCtx   context.Context

	snapshotMirrorConn             *net.UDPConn
	snapshotMirrorPort             int
	snapshotMirrorReady            bool
	snapshotMirrorSPS              bool
	snapshotMirrorPPS              bool
	snapshotMirrorWarmupDoneFrames int
	snapshotMirrorWaitForIDR       bool

	onVideoPacketRTP func(*rtp.Packet)
	onAudioPacketRTP func(*rtp.Packet)
}

func NewServer(cfg config.Config, logger *log.Logger, lifecycle Lifecycle) *Server {
	paths := entrypoint.RTSPRoutes(cfg.Entrypoints)

	s := &Server{
		cfg:         cfg,
		logger:      logger,
		transport:   NewTransport(lifecycle),
		readers:     map[*gortsplib.ServerSession]readerInfo{},
		paths:       paths,
		pathList:    sortedRoutePaths(paths),
		returnAudio: newReturnAudioForwarder(btReturnAudioAddr),
		audioBridge: audiobridge.New(audiobridge.DefaultConfig(cfg.DataDir), logger),
	}
	s.srv = &gortsplib.Server{
		Handler:        s,
		RTSPAddress:    strings.TrimSpace(cfg.MediaRTSPAddress),
		UDPRTPAddress:  ":8000",
		UDPRTCPAddress: ":8001",
	}
	return s
}

func (s *Server) Start(ctx context.Context) error {
	if err := s.srv.Start(); err != nil {
		return fmt.Errorf("start rtsp server: %w", err)
	}
	s.logf("rtsp server started addr=%s paths=%v", s.cfg.MediaRTSPAddress, s.pathList)

	if err := s.ensureStaticStream(); err != nil {
		s.srv.Close()
		return fmt.Errorf("initialize static stream: %w", err)
	}
	s.bridgeCtx = ctx

	go func() {
		<-ctx.Done()
		if s.audioBridge.Enabled() {
			_ = s.audioBridge.Stop(context.Background())
		}
		s.closeReturnAudio()
		s.closeSnapshotMirror()
		s.srv.Close()
	}()

	go func() {
		if err := s.srv.Wait(); err != nil {
			s.logf("rtsp server stopped: %v", err)
		}
	}()

	go s.runIngestListener(ctx, s.cfg.MediaRTPVideoPort, description.MediaTypeVideo)
	if s.audioBridge.Enabled() {
		ports := s.audioBridge.Ports()
		go s.runIngestListener(ctx, s.cfg.MediaRTPAudioPort, description.MediaTypeAudio)
		go s.runBridgeOpusOutListener(ctx, ports.OpusOut, s.audioBridge.OpusPayloadType())
		go s.runBridgeSpeexOutListener(ctx, ports.SpeexOut)
	} else {
		go s.runIngestListener(ctx, s.cfg.MediaRTPAudioPort, description.MediaTypeAudio)
	}
	go s.watchReaderSessions(ctx)

	return nil
}

func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !s.isKnownPath(ctx.Path) {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !s.isKnownPath(ctx.Path) {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	if ctx.Session.State() == gortsplib.ServerSessionStatePreRecord {
		return &base.Response{StatusCode: base.StatusOK}, nil, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *Server) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	entrypointID, devAddr, ok := s.resolveEntrypoint(ctx.Path)
	if !ok {
		s.logf("rtsp play rejected path=%s reason=unknown_path", ctx.Path)
		return &base.Response{StatusCode: base.StatusNotFound}, nil
	}
	sessionID := sessionID(ctx.Session)
	s.logf("rtsp play request session=%s path=%s entrypoint=%s devaddr=%s", sessionID, ctx.Path, entrypointID, devAddr)

	s.mu.Lock()
	if existing, exists := s.readers[ctx.Session]; exists {
		if existing.EntrypointID == entrypointID {
			existing.LastSeen = time.Now()
			s.readers[ctx.Session] = existing
			s.mu.Unlock()
			s.logf("rtsp play noop session=%s entrypoint=%s reason=already_reader", sessionID, entrypointID)
			return &base.Response{StatusCode: base.StatusOK}, nil
		}
		s.mu.Unlock()
		s.logf("rtsp play rejected session=%s entrypoint=%s existing_entrypoint=%s reason=session_entrypoint_mismatch", sessionID, entrypointID, existing.EntrypointID)
		return &base.Response{StatusCode: base.StatusBadRequest}, nil
	}
	s.mu.Unlock()

	startCtx, cancel := context.WithTimeout(context.Background(), streamAutostartTimeout)
	err := s.transport.OnPlay(startCtx, sessionID, entrypointID, devAddr)
	cancel()
	if err != nil {
		s.logf("rtsp stream autostart failed session=%s entrypoint=%s devaddr=%s err=%v", sessionID, entrypointID, devAddr, err)
		return &base.Response{StatusCode: base.StatusBadRequest}, nil
	}

	s.mu.Lock()
	s.readers[ctx.Session] = readerInfo{
		SessionID:    sessionID,
		EntrypointID: entrypointID,
		DevAddr:      devAddr,
		LastSeen:     time.Now(),
	}
	s.mu.Unlock()
	s.logf("rtsp play accepted session=%s entrypoint=%s devaddr=%s", sessionID, entrypointID, devAddr)

	ctx.Session.OnPacketRTPAny(func(medi *description.Media, _ format.Format, pkt *rtp.Packet) {
		if medi != s.backMed {
			return
		}
		s.forwardReturnAudio(ctx.Session, pkt)
	})

	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (s *Server) OnPause(ctx *gortsplib.ServerHandlerOnPauseCtx) (*base.Response, error) {
	s.logf("rtsp pause request session=%s", sessionID(ctx.Session))
	s.removeReader(ctx.Session)
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (s *Server) OnGetParameter(ctx *gortsplib.ServerHandlerOnGetParameterCtx) (*base.Response, error) {
	s.touchReader(ctx.Session)
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (s *Server) OnSetParameter(ctx *gortsplib.ServerHandlerOnSetParameterCtx) (*base.Response, error) {
	s.touchReader(ctx.Session)
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.logf("rtsp session close session=%s", sessionID(ctx.Session))
	s.removeReader(ctx.Session)
}

func (s *Server) SetOnVideoPacketRTP(fn func(*rtp.Packet)) {
	s.mu.Lock()
	s.onVideoPacketRTP = fn
	s.mu.Unlock()
}

func (s *Server) SetOnAudioPacketRTP(fn func(*rtp.Packet)) {
	s.mu.Lock()
	s.onAudioPacketRTP = fn
	s.mu.Unlock()
}

func (s *Server) ensureStaticStream() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream != nil {
		return nil
	}

	desc, videoMedia, audioMedia, backMedia := buildStaticStreamDescription(
		s.audioBridge.Enabled(),
		s.audioBridge.OpusPayloadType(),
		s.audioBridge.BackchannelOpusPayloadType(),
	)
	stream := &gortsplib.ServerStream{
		Server: s.srv,
		Desc:   desc,
	}
	if err := stream.Initialize(); err != nil {
		return err
	}
	s.stream = stream
	s.videoMed = videoMedia
	s.audioMed = audioMedia
	s.backMed = backMedia
	return nil
}

func buildStaticStreamDescription(audioBridgeEnabled bool, opusPayloadType uint8, backchannelOpusPayloadType uint8) (*description.Session, *description.Media, *description.Media, *description.Media) {
	videoForma := &format.H264{
		PayloadTyp:        rtpPayloadTypeH264,
		PacketizationMode: 1,
	}
	var audioForma format.Format
	var backForma format.Format
	if audioBridgeEnabled {
		audioForma = &format.Opus{
			PayloadTyp: opusPayloadType,
		}
		backForma = &format.Opus{
			PayloadTyp: backchannelOpusPayloadType,
		}
	} else {
		audioForma = &format.Speex{
			PayloadTyp: rtpPayloadTypeSpeex,
			SampleRate: rtpSpeexSampleRate,
		}
		backForma = &format.Speex{
			PayloadTyp: rtpPayloadTypeSpeexBackchannel,
			SampleRate: rtpSpeexSampleRate,
		}
	}
	videoMedia := &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{videoForma},
	}
	audioMedia := &description.Media{
		Type:    description.MediaTypeAudio,
		Formats: []format.Format{audioForma},
	}
	backMedia := &description.Media{
		Type:          description.MediaTypeAudio,
		IsBackChannel: true,
		Formats:       []format.Format{backForma},
	}
	desc := &description.Session{
		Medias: []*description.Media{videoMedia, audioMedia, backMedia},
	}

	return desc, videoMedia, audioMedia, backMedia
}

func (s *Server) runIngestListener(ctx context.Context, port int, mediaType description.MediaType) {
	if port <= 0 || port > 65535 {
		return
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		s.logf("rtsp ingest listener failed media=%v port=%d err=%v", mediaType, port, err)
		return
	}
	defer conn.Close()
	_ = conn.SetReadBuffer(1 << 20)
	s.logf("rtsp ingest listener started media=%v addr=%s", mediaType, conn.LocalAddr().String())

	buf := make([]byte, 2048)
	validPackets := 0
	lastCountLog := time.Now()
	for {
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			s.logf("rtsp ingest set deadline failed media=%v err=%v", mediaType, err)
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				select {
				case <-ctx.Done():
					return
				default:
					continue
				}
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			s.logf("rtsp ingest read error media=%v port=%d err=%v", mediaType, port, err)
			continue
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		if isExpectedPayloadType(mediaType, pkt.PayloadType) {
			validPackets++
			if validPackets == 1 {
				s.logf("rtsp ingest first_packet media=%v port=%d pt=%d seq=%d ts=%d ssrc=%d", mediaType, port, pkt.PayloadType, pkt.SequenceNumber, pkt.Timestamp, pkt.SSRC)
			} else if time.Since(lastCountLog) >= 5*time.Second {
				s.logf("rtsp ingest packets media=%v port=%d count=%d last_seq=%d last_ts=%d", mediaType, port, validPackets, pkt.SequenceNumber, pkt.Timestamp)
				lastCountLog = time.Now()
			}
		}
		s.writeIngestPacket(mediaType, &pkt)
	}
}

func (s *Server) writeIngestPacket(mediaType description.MediaType, pkt *rtp.Packet) {
	if pkt == nil {
		return
	}
	if !isExpectedPayloadType(mediaType, pkt.PayloadType) {
		return
	}
	if mediaType == description.MediaTypeAudio && s.audioBridge.Enabled() {
		if err := s.audioBridge.WriteIntercomSpeex(pkt); err != nil {
			s.logf("audio bridge ingest failed: %v", err)
		}
		return
	}
	if mediaType == description.MediaTypeVideo {
		s.writeSnapshotMirror(pkt)
		if cb := s.videoPacketCallback(); cb != nil {
			cb(pkt)
		}
	} else if mediaType == description.MediaTypeAudio {
		if cb := s.audioPacketCallback(); cb != nil {
			cb(pkt)
		}
	}

	s.mu.RLock()
	stream := s.stream
	videoMed := s.videoMed
	audioMed := s.audioMed
	s.mu.RUnlock()
	if stream == nil {
		return
	}

	var media *description.Media
	switch mediaType {
	case description.MediaTypeVideo:
		media = videoMed
	case description.MediaTypeAudio:
		media = audioMed
	}
	if media == nil {
		return
	}
	if err := stream.WritePacketRTP(media, pkt); err != nil {
		s.logf("rtsp ingest write packet error: %v", err)
	}
}

func isExpectedPayloadType(mediaType description.MediaType, payloadType uint8) bool {
	switch mediaType {
	case description.MediaTypeVideo:
		return payloadType == rtpPayloadTypeH264
	case description.MediaTypeAudio:
		return payloadType == rtpPayloadTypeSpeex
	default:
		return false
	}
}

func (s *Server) forwardReturnAudio(sess *gortsplib.ServerSession, pkt *rtp.Packet) {
	if pkt == nil {
		return
	}

	s.mu.RLock()
	_, hasReader := s.readers[sess]
	s.mu.RUnlock()
	if !hasReader {
		return
	}

	if s.audioBridge.Enabled() {
		if pkt.PayloadType != s.audioBridge.BackchannelOpusPayloadType() {
			return
		}
		if err := s.audioBridge.WriteBackchannelOpus(pkt); err != nil {
			s.logf("rtsp backchannel bridge write failed: %v", err)
		}
		return
	}
	if pkt.PayloadType != rtpPayloadTypeSpeexBackchannel {
		return
	}
	if err := s.returnAudio.WriteRTP(pkt); err != nil {
		s.logf("rtsp backchannel forward failed: %v", err)
	}
}

func (s *Server) runBridgeOpusOutListener(ctx context.Context, port int, expectedPayloadType uint8) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		s.logf("audio bridge opus output listener failed port=%d err=%v", port, err)
		return
	}
	defer conn.Close()
	_ = conn.SetReadBuffer(1 << 20)

	buf := make([]byte, 2048)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			s.logf("audio bridge opus output deadline failed err=%v", err)
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				select {
				case <-ctx.Done():
					return
				default:
					continue
				}
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			s.logf("audio bridge opus output read failed err=%v", err)
			continue
		}

		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		if pkt.PayloadType != expectedPayloadType {
			continue
		}
		if cb := s.audioPacketCallback(); cb != nil {
			cb(&pkt)
		}

		s.mu.RLock()
		stream := s.stream
		audioMed := s.audioMed
		s.mu.RUnlock()
		if stream == nil || audioMed == nil {
			continue
		}
		if err := stream.WritePacketRTP(audioMed, &pkt); err != nil {
			s.logf("audio bridge opus stream write failed: %v", err)
		}
	}
}

func (s *Server) WriteBackchannelOpus(pkt *rtp.Packet) error {
	if pkt == nil {
		return nil
	}

	if s.audioBridge.Enabled() {
		pkt.PayloadType = s.audioBridge.BackchannelOpusPayloadType()
		return s.audioBridge.WriteBackchannelOpus(pkt)
	}

	pkt.PayloadType = rtpPayloadTypeSpeexBackchannel
	return s.returnAudio.WriteRTP(pkt)
}

func (s *Server) BackchannelOpusPayloadType() uint8 {
	if s.audioBridge == nil {
		return rtpPayloadTypeSpeexBackchannel
	}
	return s.audioBridge.BackchannelOpusPayloadType()
}

func (s *Server) OpusPayloadType() uint8 {
	if s.audioBridge == nil {
		return rtpPayloadTypeSpeex
	}
	return s.audioBridge.OpusPayloadType()
}

func (s *Server) runBridgeSpeexOutListener(ctx context.Context, port int) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		s.logf("audio bridge speex output listener failed port=%d err=%v", port, err)
		return
	}
	defer conn.Close()
	_ = conn.SetReadBuffer(1 << 20)

	buf := make([]byte, 2048)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			s.logf("audio bridge speex output deadline failed err=%v", err)
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				select {
				case <-ctx.Done():
					return
				default:
					continue
				}
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			s.logf("audio bridge speex output read failed err=%v", err)
			continue
		}

		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		if pkt.PayloadType != rtpPayloadTypeSpeexBackchannel {
			continue
		}
		if err := s.returnAudio.WriteRTP(&pkt); err != nil {
			s.logf("audio bridge speex forward failed: %v", err)
		}
	}
}

func (s *Server) closeReturnAudio() {
	if s.returnAudio != nil {
		s.returnAudio.Close()
	}
}

func (s *Server) BeginSnapshotMirror() (int, func(), error) {
	port, err := reserveLocalUDPPort()
	if err != nil {
		return 0, nil, err
	}
	dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	conn, err := net.DialUDP("udp4", nil, dst)
	if err != nil {
		return 0, nil, err
	}

	s.mu.Lock()
	if s.snapshotMirrorConn != nil {
		s.mu.Unlock()
		_ = conn.Close()
		return 0, nil, ErrSnapshotMirrorBusy
	}
	s.snapshotMirrorConn = conn
	s.snapshotMirrorPort = port
	s.snapshotMirrorReady = false
	s.snapshotMirrorSPS = false
	s.snapshotMirrorPPS = false
	s.snapshotMirrorWarmupDoneFrames = 0
	s.snapshotMirrorWaitForIDR = false
	s.mu.Unlock()

	stop := func() {
		s.mu.Lock()
		if s.snapshotMirrorConn == conn {
			_ = s.snapshotMirrorConn.Close()
			s.snapshotMirrorConn = nil
			s.snapshotMirrorPort = 0
			s.snapshotMirrorReady = false
			s.snapshotMirrorSPS = false
			s.snapshotMirrorPPS = false
			s.snapshotMirrorWarmupDoneFrames = 0
			s.snapshotMirrorWaitForIDR = false
		}
		s.mu.Unlock()
	}

	return port, stop, nil
}

func (s *Server) writeSnapshotMirror(pkt *rtp.Packet) {
	s.mu.Lock()
	conn := s.snapshotMirrorConn
	if conn == nil || pkt == nil {
		s.mu.Unlock()
		return
	}
	if !s.snapshotMirrorReady {
		sawSPS, sawPPS, sawIDR := inspectH264NALTypes(pkt.Payload)
		if sawSPS {
			s.snapshotMirrorSPS = true
		}
		if sawPPS {
			s.snapshotMirrorPPS = true
		}
		if !s.snapshotMirrorSPS || !s.snapshotMirrorPPS {
			s.mu.Unlock()
			return
		}
		if s.snapshotMirrorWarmupDoneFrames < snapshotWarmupFrames {
			if pkt.Marker {
				s.snapshotMirrorWarmupDoneFrames++
				if s.snapshotMirrorWarmupDoneFrames >= snapshotWarmupFrames {
					s.snapshotMirrorWaitForIDR = true
				}
			}
			s.mu.Unlock()
			return
		}
		if s.snapshotMirrorWaitForIDR {
			if !sawIDR {
				s.mu.Unlock()
				return
			}
			s.snapshotMirrorWaitForIDR = false
		}
		s.snapshotMirrorReady = true
	}
	s.mu.Unlock()
	raw, err := pkt.Marshal()
	if err != nil {
		return
	}
	_, _ = conn.Write(raw)
}

func (s *Server) closeSnapshotMirror() {
	s.mu.Lock()
	if s.snapshotMirrorConn != nil {
		_ = s.snapshotMirrorConn.Close()
		s.snapshotMirrorConn = nil
		s.snapshotMirrorPort = 0
		s.snapshotMirrorReady = false
		s.snapshotMirrorSPS = false
		s.snapshotMirrorPPS = false
		s.snapshotMirrorWarmupDoneFrames = 0
		s.snapshotMirrorWaitForIDR = false
	}
	s.mu.Unlock()
}

func inspectH264NALTypes(payload []byte) (bool, bool, bool) {
	if len(payload) == 0 {
		return false, false, false
	}

	sawSPS := false
	sawPPS := false
	sawIDR := false

	applyNALType := func(nalType uint8) {
		switch nalType {
		case h264NALTypeSPS:
			sawSPS = true
		case h264NALTypePPS:
			sawPPS = true
		case h264NALTypeIDR:
			sawIDR = true
		}
	}

	nalType := payload[0] & 0x1f
	switch nalType {
	case h264NALTypeSTAPA:
		offset := 1
		for offset+2 <= len(payload) {
			size := int(payload[offset])<<8 | int(payload[offset+1])
			offset += 2
			if size <= 0 || offset+size > len(payload) {
				break
			}
			applyNALType(payload[offset] & 0x1f)
			offset += size
		}
	case h264NALTypeFUA:
		if len(payload) < 2 {
			return false, false, false
		}
		start := (payload[1] & 0x80) != 0
		if start {
			applyNALType(payload[1] & 0x1f)
		}
	default:
		if nalType > 0 && nalType <= 23 {
			applyNALType(nalType)
		}
	}
	return sawSPS, sawPPS, sawIDR
}

func reserveLocalUDPPort() (int, error) {
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return 0, err
	}
	defer listener.Close()

	addr, ok := listener.LocalAddr().(*net.UDPAddr)
	if !ok || addr == nil || addr.Port <= 0 {
		return 0, errors.New("unable to reserve local udp port")
	}
	return addr.Port, nil
}

func (s *Server) watchReaderSessions(ctx context.Context) {
	ticker := time.NewTicker(readerWatchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			var stale []*gortsplib.ServerSession
			s.mu.RLock()
			for sess, info := range s.readers {
				if now.Sub(info.LastSeen) >= readerIdleTimeout {
					stale = append(stale, sess)
				}
			}
			s.mu.RUnlock()
			for _, sess := range stale {
				s.logf("rtsp reader watchdog closing stale session idle_for=%s", readerIdleTimeout)
				s.removeReader(sess)
				sess.Close()
			}
		}
	}
}

func (s *Server) removeReader(sess *gortsplib.ServerSession) {
	if sess == nil {
		return
	}
	s.mu.Lock()
	info, ok := s.readers[sess]
	if ok {
		delete(s.readers, sess)
	}
	s.mu.Unlock()
	if ok {
		s.logf("rtsp reader remove session=%s entrypoint=%s devaddr=%s", info.SessionID, info.EntrypointID, info.DevAddr)
		stopCtx, cancel := context.WithTimeout(context.Background(), streamAutostopTimeout)
		if err := s.transport.OnPause(stopCtx, info.SessionID); err != nil {
			s.logf("rtsp reader pause lifecycle failed session=%s err=%v", info.SessionID, err)
		}
		cancel()
	}
}

func (s *Server) OnStreamStarted() {
	if s == nil || !s.audioBridge.Enabled() {
		return
	}
	s.mu.RLock()
	bridgeCtx := s.bridgeCtx
	s.mu.RUnlock()
	if bridgeCtx == nil {
		return
	}
	if err := s.audioBridge.Start(bridgeCtx); err != nil {
		s.logf("audio bridge start failed: %v", err)
	}
}

func (s *Server) OnStreamStopped() {
	if s == nil || !s.audioBridge.Enabled() {
		return
	}
	if err := s.audioBridge.Stop(context.Background()); err != nil {
		s.logf("audio bridge stop failed: %v", err)
	}
}

func (s *Server) touchReader(sess *gortsplib.ServerSession) {
	if sess == nil {
		return
	}
	s.mu.Lock()
	info, ok := s.readers[sess]
	if ok {
		info.LastSeen = time.Now()
		s.readers[sess] = info
	}
	s.mu.Unlock()
	if ok {
		s.transport.OnGetParameter(info.SessionID)
	}
}

func (s *Server) resolveEntrypoint(path string) (string, string, bool) {
	reqPath := strings.TrimPrefix(strings.TrimSpace(path), "/")
	route, ok := s.paths[reqPath]
	if !ok {
		return "", "", false
	}
	return route.EntrypointID, route.DevAddr, true
}

func (s *Server) isKnownPath(path string) bool {
	reqPath := strings.TrimPrefix(strings.TrimSpace(path), "/")
	_, ok := s.paths[reqPath]
	return ok
}

func sessionID(session *gortsplib.ServerSession) string {
	return fmt.Sprintf("%p", session)
}

func (s *Server) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}

func (s *Server) videoPacketCallback() func(*rtp.Packet) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.onVideoPacketRTP
}

func (s *Server) audioPacketCallback() func(*rtp.Packet) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.onAudioPacketRTP
}

func sortedRoutePaths(routes map[string]entrypoint.StreamRoute) []string {
	paths := make([]string, 0, len(routes))
	for path := range routes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

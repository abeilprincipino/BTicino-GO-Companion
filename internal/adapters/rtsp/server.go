package rtspadapter

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"bticino-go-companion/internal/config"
)

const (
	streamAutostartTimeout = 12 * time.Second
	streamAutostopTimeout  = 6 * time.Second
	readerWatchdogInterval = 10 * time.Second
	readerIdleTimeout      = 40 * time.Second
)

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
	readers  map[*gortsplib.ServerSession]readerInfo
	mainPath string
}

func NewServer(cfg config.Config, logger *log.Logger, lifecycle Lifecycle) *Server {
	mainPath := normalizePath(cfg.MediaRTSPPathMain, "doorbell")

	s := &Server{
		cfg:       cfg,
		logger:    logger,
		transport: NewTransport(lifecycle),
		readers:   map[*gortsplib.ServerSession]readerInfo{},
		mainPath:  mainPath,
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
	s.logf("rtsp server started addr=%s path_main=/%s", s.cfg.MediaRTSPAddress, s.mainPath)

	if err := s.ensureStaticStream(); err != nil {
		s.srv.Close()
		return fmt.Errorf("initialize static stream: %w", err)
	}

	go func() {
		<-ctx.Done()
		s.srv.Close()
	}()

	go func() {
		if err := s.srv.Wait(); err != nil {
			s.logf("rtsp server stopped: %v", err)
		}
	}()

	go s.runIngestListener(ctx, s.cfg.MediaRTPVideoPort, description.MediaTypeVideo)
	go s.runIngestListener(ctx, s.cfg.MediaRTPAudioPort, description.MediaTypeAudio)
	go s.watchReaderSessions(ctx)

	return nil
}

func (s *Server) StreamURL(host string) string {
	return fmt.Sprintf("rtsp://%s/%s", net.JoinHostPort(extractHost(host), extractPortString(s.cfg.MediaRTSPAddress, 8554)), s.mainPath)
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
	entrypointID, devAddr := s.resolveEntrypoint(ctx.Path)
	sessionID := sessionID(ctx.Session)

	startCtx, cancel := context.WithTimeout(context.Background(), streamAutostartTimeout)
	err := s.transport.OnPlay(startCtx, sessionID, entrypointID, devAddr)
	cancel()
	if err != nil {
		s.logf("rtsp stream autostart failed: %v", err)
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

	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (s *Server) OnPause(ctx *gortsplib.ServerHandlerOnPauseCtx) (*base.Response, error) {
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
	s.removeReader(ctx.Session)
}

func (s *Server) ensureStaticStream() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream != nil {
		return nil
	}

	videoForma := &format.H264{
		PayloadTyp:        96,
		PacketizationMode: 1,
	}
	audioForma := &format.Speex{
		PayloadTyp: 110,
		SampleRate: 8000,
	}
	videoMedia := &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{videoForma},
	}
	audioMedia := &description.Media{
		Type:    description.MediaTypeAudio,
		Formats: []format.Format{audioForma},
	}
	desc := &description.Session{
		Medias: []*description.Media{videoMedia, audioMedia},
	}

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
	return nil
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
		return payloadType == 96
	case description.MediaTypeAudio:
		return payloadType == 110
	default:
		return false
	}
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
		stopCtx, cancel := context.WithTimeout(context.Background(), streamAutostopTimeout)
		_ = s.transport.OnPause(stopCtx, info.SessionID)
		cancel()
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

func (s *Server) resolveEntrypoint(path string) (string, string) {
	main := strings.TrimPrefix(s.mainPath, "/")
	reqPath := strings.TrimPrefix(strings.TrimSpace(path), "/")

	if reqPath == main {
		for _, ep := range s.cfg.Entrypoints {
			if ep.HasStream {
				return ep.ID, ep.DevAddr
			}
		}
	}
	if len(s.cfg.Entrypoints) > 0 {
		return s.cfg.Entrypoints[0].ID, s.cfg.Entrypoints[0].DevAddr
	}
	return "main", "20"
}

func (s *Server) isKnownPath(path string) bool {
	reqPath := strings.TrimPrefix(strings.TrimSpace(path), "/")
	main := strings.TrimPrefix(s.mainPath, "/")
	return reqPath == main
}

func normalizePath(raw string, fallback string) string {
	val := strings.TrimSpace(raw)
	val = strings.TrimPrefix(val, "/")
	if val == "" {
		return fallback
	}
	return val
}

func sessionID(session *gortsplib.ServerSession) string {
	return fmt.Sprintf("%p", session)
}

func extractHost(raw string) string {
	trimmed := strings.TrimSpace(raw)
	host, _, err := net.SplitHostPort(trimmed)
	if err == nil {
		return strings.Trim(host, "[]")
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return strings.Trim(trimmed, "[]")
	}
	if trimmed == "" {
		return "127.0.0.1"
	}
	return trimmed
}

func extractPortString(raw string, fallback int) string {
	addr := strings.TrimSpace(raw)
	if strings.HasPrefix(addr, ":") {
		p := strings.TrimPrefix(addr, ":")
		if p != "" {
			return p
		}
	}
	_, p, err := net.SplitHostPort(addr)
	if err == nil && strings.TrimSpace(p) != "" {
		return strings.TrimSpace(p)
	}
	return fmt.Sprintf("%d", fallback)
}

func (s *Server) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}

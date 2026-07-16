package media

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"
)

const DefaultRTSPAddress = ":8554"

var (
	ErrSourceUnavailable       = errors.New("media: rtsp source is unavailable")
	ErrEntrypointSwitchBlocked = errors.New("media: another entrypoint source is active")
)

type RTSPSource interface {
	Start(context.Context) error
	Close(context.Context) error
}

// RTSPSourceFactory creates the selected entrypoint's source and forwards H.264 RTP to packet.
type RTSPSourceFactory func(config.Entrypoint, func(*rtp.Packet)) (RTSPSource, func(), error)

type rtspRoute struct {
	entrypoint config.Entrypoint
}

type rtspReader struct {
	route rtspRoute
}

// RTSPServer maps every configured stream entrypoint to doorbell-<entrypoint-id>.
// The device has one fixed RTP input, so readers can share one entrypoint source only.
type RTSPServer struct {
	mu sync.Mutex

	logger        *slog.Logger
	address       string
	routes        map[string]rtspRoute
	sourceFactory RTSPSourceFactory
	server        *gortsplib.Server
	ctx           context.Context

	stream  *gortsplib.ServerStream
	video   *description.Media
	source  RTSPSource
	cleanup func()
	active  rtspRoute
	readers map[*gortsplib.ServerSession]rtspReader
}

func NewRTSPServer(logger *slog.Logger, address string, entrypoints []config.Entrypoint, sourceFactory RTSPSourceFactory) (*RTSPServer, error) {
	if sourceFactory == nil {
		return nil, fmt.Errorf("media: rtsp source factory is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if address == "" {
		address = DefaultRTSPAddress
	}
	routes := make(map[string]rtspRoute)
	for _, entrypoint := range entrypoints {
		if !entrypoint.Capabilities.Stream {
			continue
		}
		path := "doorbell-" + rtspPathToken(entrypoint.ID)
		if path == "doorbell-" {
			return nil, fmt.Errorf("media: rtsp stream entrypoint id is required")
		}
		if _, exists := routes[path]; exists {
			return nil, fmt.Errorf("media: duplicate rtsp path %q", path)
		}
		routes[path] = rtspRoute{entrypoint: entrypoint}
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("media: no stream-capable entrypoints")
	}
	return &RTSPServer{
		logger:        logger.With("component", "media.rtsp"),
		address:       address,
		routes:        routes,
		sourceFactory: sourceFactory,
		readers:       make(map[*gortsplib.ServerSession]rtspReader),
	}, nil
}

func (s *RTSPServer) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server != nil {
		return fmt.Errorf("media: rtsp server already started")
	}
	s.ctx = ctx
	s.server = &gortsplib.Server{Handler: s, RTSPAddress: s.address}
	if err := s.server.Start(); err != nil {
		s.server = nil
		return fmt.Errorf("start rtsp server: %w", err)
	}
	s.logger.InfoContext(ctx, "rtsp server listening", "address", s.address, "paths", s.paths())
	go func() {
		<-ctx.Done()
		if err := s.Close(); err != nil {
			s.logger.Warn("close rtsp server", "error", err)
		}
	}()
	return nil
}

func (s *RTSPServer) Close() error {
	s.mu.Lock()
	server, stream := s.server, s.stream
	s.server, s.stream, s.video = nil, nil, nil
	s.readers = make(map[*gortsplib.ServerSession]rtspReader)
	source, cleanup := s.takeSourceLocked()
	s.mu.Unlock()
	if err := stopRTSPSource(source, cleanup); err != nil {
		return err
	}
	if stream != nil {
		stream.Close()
	}
	if server != nil {
		server.Close()
	}
	return nil
}

func (s *RTSPServer) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !s.knownPath(ctx.Path) {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureStreamLocked(); err != nil {
		s.logger.Error("create rtsp stream", "error", err)
		return &base.Response{StatusCode: base.StatusInternalServerError}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *RTSPServer) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !s.knownPath(ctx.Path) {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureStreamLocked(); err != nil {
		return &base.Response{StatusCode: base.StatusInternalServerError}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *RTSPServer) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	route, ok := s.route(ctx.Path)
	if !ok {
		return &base.Response{StatusCode: base.StatusNotFound}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if reader, exists := s.readers[ctx.Session]; exists {
		if reader.route.entrypoint.ID != route.entrypoint.ID {
			return &base.Response{StatusCode: base.StatusBadRequest}, ErrEntrypointSwitchBlocked
		}
		return &base.Response{StatusCode: base.StatusOK}, nil
	}
	if s.source != nil && s.active.entrypoint.ID != route.entrypoint.ID {
		s.logger.Warn("rtsp play rejected; another entrypoint is active", "requested_entrypoint_id", route.entrypoint.ID, "active_entrypoint_id", s.active.entrypoint.ID)
		return &base.Response{StatusCode: base.StatusBadRequest}, ErrEntrypointSwitchBlocked
	}
	if err := s.startSourceLocked(route); err != nil {
		s.logger.Error("start rtsp source", "entrypoint_id", route.entrypoint.ID, "dev_addr", route.entrypoint.DevAddr, "error", err)
		return &base.Response{StatusCode: base.StatusInternalServerError}, nil
	}
	s.readers[ctx.Session] = rtspReader{route: route}
	s.logger.Info("rtsp reader started", "entrypoint_id", route.entrypoint.ID, "dev_addr", route.entrypoint.DevAddr, "readers", len(s.readers))
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (s *RTSPServer) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.mu.Lock()
	reader, exists := s.readers[ctx.Session]
	if !exists {
		s.mu.Unlock()
		return
	}
	delete(s.readers, ctx.Session)
	if len(s.readers) != 0 {
		readers := len(s.readers)
		s.mu.Unlock()
		s.logger.Info("rtsp reader stopped", "entrypoint_id", reader.route.entrypoint.ID, "readers", readers)
		return
	}
	source, cleanup := s.takeSourceLocked()
	s.mu.Unlock()
	s.logger.Info("rtsp last reader stopped; stopping source", "entrypoint_id", reader.route.entrypoint.ID)
	if err := stopRTSPSource(source, cleanup); err != nil {
		s.logger.Warn("stop rtsp source", "error", err)
	}
}

func (s *RTSPServer) ensureStreamLocked() error {
	if s.stream != nil {
		return nil
	}
	if s.server == nil {
		return fmt.Errorf("rtsp server is not started")
	}
	s.video = &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: VideoPayloadType, PacketizationMode: 1}}}
	s.stream = &gortsplib.ServerStream{Server: s.server, Desc: &description.Session{Medias: []*description.Media{s.video}}}
	if err := s.stream.Initialize(); err != nil {
		s.stream, s.video = nil, nil
		return fmt.Errorf("initialize h264 rtsp stream: %w", err)
	}
	return nil
}

func (s *RTSPServer) startSourceLocked(route rtspRoute) error {
	if s.source != nil {
		return nil
	}
	source, cleanup, err := s.sourceFactory(route.entrypoint, s.writeVideoRTP)
	if err != nil {
		return fmt.Errorf("create source: %w", err)
	}
	if source == nil {
		return ErrSourceUnavailable
	}
	if err := source.Start(s.ctx); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return fmt.Errorf("start source: %w", err)
	}
	s.source, s.cleanup, s.active = source, cleanup, route
	s.logger.Info("rtsp source started", "entrypoint_id", route.entrypoint.ID, "dev_addr", route.entrypoint.DevAddr)
	return nil
}

func (s *RTSPServer) takeSourceLocked() (RTSPSource, func()) {
	source, cleanup := s.source, s.cleanup
	s.source, s.cleanup, s.active = nil, nil, rtspRoute{}
	return source, cleanup
}

func (s *RTSPServer) writeVideoRTP(packet *rtp.Packet) {
	if packet == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream != nil {
		s.stream.WritePacketRTP(s.video, packet)
	}
}

func (s *RTSPServer) knownPath(path string) bool {
	_, ok := s.route(path)
	return ok
}

func (s *RTSPServer) route(path string) (rtspRoute, bool) {
	route, ok := s.routes[strings.TrimPrefix(strings.TrimSpace(path), "/")]
	return route, ok
}

func (s *RTSPServer) paths() []string {
	paths := make([]string, 0, len(s.routes))
	for path := range s.routes {
		paths = append(paths, path)
	}
	return paths
}

func rtspPathToken(id string) string {
	value := strings.ToLower(strings.TrimSpace(id))
	var token strings.Builder
	lastDash := false
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '-', char == '_':
			token.WriteRune(char)
			lastDash = false
		default:
			if !lastDash {
				token.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(token.String(), "-")
}

func stopRTSPSource(source RTSPSource, cleanup func()) error {
	if source == nil {
		return nil
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := source.Close(closeCtx)
	cancel()
	if cleanup != nil {
		cleanup()
	}
	if err != nil {
		return fmt.Errorf("stop rtsp source: %w", err)
	}
	return nil
}

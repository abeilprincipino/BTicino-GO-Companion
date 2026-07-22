package media

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

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

type rtspRoute struct {
	entrypoint config.Entrypoint
}

type rtspReader struct {
	route rtspRoute
	lease *StreamLease
}

type rtspStartingReader struct {
	cancel context.CancelFunc
}

// RTSPServer maps every configured stream entrypoint to doorbell-<entrypoint-id>.
// The device has one fixed RTP input, so readers can share one entrypoint source only.
type RTSPServer struct {
	mu sync.Mutex

	logger      *slog.Logger
	address     string
	routes      map[string]rtspRoute
	server      *gortsplib.Server
	ctx         context.Context
	coordinator *StreamCoordinator

	stream   *gortsplib.ServerStream
	video    *description.Media
	audio    *description.Media
	readers  map[*gortsplib.ServerSession]rtspReader
	starting map[*gortsplib.ServerSession]rtspStartingReader
}

func NewRTSPServer(logger *slog.Logger, address string, entrypoints []config.Entrypoint, sourceFactory ManagedSourceFactory) (*RTSPServer, error) {
	if sourceFactory == nil {
		return nil, errors.New("media: rtsp source factory is required")
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
			return nil, errors.New("media: rtsp stream entrypoint id is required")
		}

		if _, exists := routes[path]; exists {
			return nil, fmt.Errorf("media: duplicate rtsp path %q", path)
		}

		routes[path] = rtspRoute{entrypoint: entrypoint}
	}

	if len(routes) == 0 {
		return nil, errors.New("media: no stream-capable entrypoints")
	}

	return &RTSPServer{
		logger:      logger.With("component", "media.rtsp"),
		address:     address,
		routes:      routes,
		readers:     make(map[*gortsplib.ServerSession]rtspReader),
		starting:    make(map[*gortsplib.ServerSession]rtspStartingReader),
		coordinator: NewStreamCoordinator(logger, sourceFactory),
	}, nil
}

func (s *RTSPServer) ObserveControlTrack(video bool) { s.coordinator.ObserveControlTrack(video) }

func (s *RTSPServer) ObserveControlStop() { s.coordinator.ObserveControlStop() }

func (s *RTSPServer) StreamSnapshot() StreamSnapshot { return s.coordinator.Snapshot() }

// Coordinator returns the coordinator shared by RTSP and WebRTC consumers.
func (s *RTSPServer) Coordinator() *StreamCoordinator { return s.coordinator }

func (s *RTSPServer) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.server != nil {
		return errors.New("media: rtsp server already started")
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
	s.server, s.stream, s.video, s.audio = nil, nil, nil, nil
	readers := s.readers
	s.readers = make(map[*gortsplib.ServerSession]rtspReader)
	starting := s.starting
	s.starting = make(map[*gortsplib.ServerSession]rtspStartingReader)
	s.mu.Unlock()

	for _, reader := range starting {
		reader.cancel()
	}

	for _, reader := range readers {
		s.coordinator.Release(reader.lease)
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
	if reader, exists := s.readers[ctx.Session]; exists {
		s.mu.Unlock()

		if reader.route.entrypoint.ID != route.entrypoint.ID {
			return &base.Response{StatusCode: base.StatusBadRequest}, ErrEntrypointSwitchBlocked
		}

		return &base.Response{StatusCode: base.StatusOK}, nil
	}

	for _, reader := range s.readers {
		if reader.route.entrypoint.ID == route.entrypoint.ID {
			s.readers[ctx.Session] = rtspReader{route: route, lease: reader.lease}
			readers := len(s.readers)
			s.mu.Unlock()
			s.logger.Info("rtsp reader started", "entrypoint_id", route.entrypoint.ID, "readers", readers, "shared_source", true)

			return &base.Response{StatusCode: base.StatusOK}, nil
		}
	}

	if _, exists := s.starting[ctx.Session]; exists {
		s.mu.Unlock()
		return &base.Response{StatusCode: base.StatusBadRequest}, ErrStreamBusy
	}

	runCtx, cancel := context.WithCancel(s.ctx)
	s.starting[ctx.Session] = rtspStartingReader{cancel: cancel}
	s.mu.Unlock()
	lease, err := s.coordinator.Acquire(runCtx, route.entrypoint, SourceEvents{
		VideoRTP: s.writeVideoRTP,
		AudioRTP: s.writeAudioRTP,
	})
	s.mu.Lock()
	_, active := s.starting[ctx.Session]
	delete(s.starting, ctx.Session)

	if err == nil && active {
		s.readers[ctx.Session] = rtspReader{route: route, lease: lease}
		readers := len(s.readers)
		s.mu.Unlock()
		s.logger.Info("rtsp reader started", "entrypoint_id", route.entrypoint.ID, "dev_addr", route.entrypoint.DevAddr, "readers", readers)

		return &base.Response{StatusCode: base.StatusOK}, nil
	}
	s.mu.Unlock()

	if err != nil {
		cancel()
		s.logger.Warn("rtsp play rejected; stream unavailable", "requested_entrypoint_id", route.entrypoint.ID, "error", err)

		return &base.Response{StatusCode: base.StatusBadRequest}, err
	}

	s.coordinator.Release(lease)

	return &base.Response{StatusCode: base.StatusBadRequest}, context.Canceled
}

func (s *RTSPServer) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.mu.Lock()

	starting, wasStarting := s.starting[ctx.Session]
	if wasStarting {
		delete(s.starting, ctx.Session)
	}

	reader, exists := s.readers[ctx.Session]
	if !exists {
		s.mu.Unlock()

		if wasStarting {
			starting.cancel()
		}

		return
	}

	delete(s.readers, ctx.Session)

	if len(s.readers) != 0 {
		readers := len(s.readers)
		s.mu.Unlock()

		if wasStarting {
			starting.cancel()
		}

		s.logger.Info("rtsp reader stopped", "entrypoint_id", reader.route.entrypoint.ID, "readers", readers)

		return
	}
	s.mu.Unlock()

	if wasStarting {
		starting.cancel()
	}

	s.coordinator.Release(reader.lease)
	s.logger.Info("rtsp reader stopped", "entrypoint_id", reader.route.entrypoint.ID)
}

func (s *RTSPServer) ensureStreamLocked() error {
	if s.stream != nil {
		return nil
	}

	if s.server == nil {
		return errors.New("rtsp server is not started")
	}

	s.video = &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{&format.H264{PayloadTyp: VideoPayloadType, PacketizationMode: 1}}}
	s.audio = &description.Media{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: audioBridgeOpusPT, ChannelCount: 1}}}

	s.stream = &gortsplib.ServerStream{Server: s.server, Desc: &description.Session{Medias: []*description.Media{s.video, s.audio}}}
	if err := s.stream.Initialize(); err != nil {
		s.stream, s.video, s.audio = nil, nil, nil
		return fmt.Errorf("initialize rtsp stream: %w", err)
	}

	return nil
}

func (s *RTSPServer) writeVideoRTP(packet *rtp.Packet) {
	s.writeRTP(true, packet)
}

func (s *RTSPServer) writeAudioRTP(packet *rtp.Packet) {
	s.writeRTP(false, packet)
}

func (s *RTSPServer) writeRTP(video bool, packet *rtp.Packet) {
	if packet == nil {
		return
	}

	s.mu.Lock()
	stream := s.stream

	media := s.audio
	if video {
		media = s.video
	}
	s.mu.Unlock()

	if stream != nil && media != nil {
		if err := stream.WritePacketRTP(media, packet); err != nil {
			s.logger.Debug("write RTSP RTP packet", "error", err)
		}
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

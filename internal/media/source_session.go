package media

import (
	"bticino-go-companion/internal/core"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

var ErrSourceSessionStarted = errors.New("media: source session already started")

type SourceSIP interface {
	StartStream(context.Context, string) error
	Hangup(context.Context) error
}

type SourceAV interface {
	Start(context.Context, bool, FlowProbe, FlowProbe) error
}

type FlowProbe interface {
	RecentlyFlowing(time.Duration) bool
}

type SourceReceiver interface {
	FlowProbe
	Start(context.Context) error
	Close() error
	Metadata() RTPMetadata
}

// SourceSession owns the SIP dialog and both RTP sockets for one device source.
type SourceSession struct {
	mu           sync.Mutex
	logger       *slog.Logger
	sourceConfig SourceConfig
	entrypointID core.EntrypointID
	sip          SourceSIP
	av           SourceAV
	video        SourceReceiver
	audio        SourceReceiver
	started      bool
}

func NewSourceSession(logger *slog.Logger, sourceConfig SourceConfig, entrypointID core.EntrypointID, sip SourceSIP, av SourceAV, video, audio SourceReceiver) *SourceSession {
	if logger == nil {
		logger = slog.Default()
	}
	return &SourceSession{logger: logger.With("component", "media.session", "model", sourceConfig.Model, "entrypoint_id", entrypointID), sourceConfig: sourceConfig, entrypointID: entrypointID, sip: sip, av: av, video: video, audio: audio}
}

func (s *SourceSession) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return ErrSourceSessionStarted
	}
	if s.sourceConfig.Model == "" || s.sourceConfig.DevAddr == "" || s.entrypointID == "" || s.sip == nil || s.av == nil || s.video == nil || s.audio == nil {
		return errors.New("media: incomplete source session")
	}
	s.logger.InfoContext(ctx, "source session starting", "dev_addr", s.sourceConfig.DevAddr, "high_res_video", s.sourceConfig.HighResVideo)
	if err := s.video.Start(ctx); err != nil {
		return fmt.Errorf("start video receiver: %w", err)
	}
	if err := s.audio.Start(ctx); err != nil {
		_ = s.video.Close()
		return fmt.Errorf("start audio receiver: %w", err)
	}
	if err := s.sip.StartStream(ctx, s.sourceConfig.DevAddr); err != nil {
		s.closeReceivers()
		return fmt.Errorf("start outgoing sip: %w", err)
	}
	if err := s.av.Start(ctx, s.sourceConfig.HighResVideo, s.video, s.audio); err != nil {
		s.closeReceivers()
		if closeErr := s.sip.Hangup(ctx); closeErr != nil {
			s.logger.WarnContext(ctx, "sip cleanup after av failure failed", "error", closeErr)
			return errors.Join(fmt.Errorf("start av: %w", err), fmt.Errorf("cleanup sip: %w", closeErr))
		}
		return fmt.Errorf("start av: %w", err)
	}
	s.started = true
	s.logger.InfoContext(ctx, "source session started")
	return nil
}

func (s *SourceSession) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return nil
	}
	s.logger.InfoContext(ctx, "source session stopping")
	s.started = false
	s.closeReceivers()
	if err := s.sip.Hangup(ctx); err != nil {
		return fmt.Errorf("stop outgoing sip: %w", err)
	}
	s.logger.InfoContext(ctx, "source session stopped")
	return nil
}

// RemoteDialogEnded releases local media after the peer terminates the SIP dialog.
// It deliberately does not send BYE because the peer has already done so.
func (s *SourceSession) RemoteDialogEnded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return
	}

	s.started = false
	s.logger.Info("source session stopped by remote sip dialog")
	s.closeReceivers()
}

func (s *SourceSession) closeReceivers() {
	if err := s.audio.Close(); err != nil {
		s.logger.Warn("close audio receiver", "error", err)
	}
	if err := s.video.Close(); err != nil {
		s.logger.Warn("close video receiver", "error", err)
	}
}

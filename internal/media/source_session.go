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
	profile      Profile
	entrypointID core.EntrypointID
	devAddr      string
	sip          SourceSIP
	av           SourceAV
	video        SourceReceiver
	audio        SourceReceiver
	started      bool
}

func NewSourceSession(logger *slog.Logger, profile Profile, entrypointID core.EntrypointID, devAddr string, sip SourceSIP, av SourceAV, video, audio SourceReceiver) *SourceSession {
	if logger == nil {
		logger = slog.Default()
	}
	return &SourceSession{logger: logger.With("component", "media.session", "model", profile.Model, "entrypoint_id", entrypointID), profile: profile, entrypointID: entrypointID, devAddr: devAddr, sip: sip, av: av, video: video, audio: audio}
}

func (s *SourceSession) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return ErrSourceSessionStarted
	}
	if s.profile.Model == "" || s.entrypointID == "" || s.devAddr == "" || s.sip == nil || s.av == nil || s.video == nil || s.audio == nil {
		return errors.New("media: incomplete source session")
	}
	if expected, err := ResolveProfile(s.profile.Model); err != nil || expected != s.profile {
		return ErrUnsupportedModel
	}
	if err := s.video.Start(ctx); err != nil {
		return fmt.Errorf("start video receiver: %w", err)
	}
	if err := s.audio.Start(ctx); err != nil {
		_ = s.video.Close()
		return fmt.Errorf("start audio receiver: %w", err)
	}
	if err := s.sip.StartStream(ctx, s.devAddr); err != nil {
		s.closeReceivers()
		return fmt.Errorf("start outgoing sip: %w", err)
	}
	if err := s.av.Start(ctx, s.profile.HighResVideo, s.video, s.audio); err != nil {
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
	s.started = false
	s.closeReceivers()
	if err := s.sip.Hangup(ctx); err != nil {
		return fmt.Errorf("stop outgoing sip: %w", err)
	}
	s.logger.InfoContext(ctx, "source session stopped")
	return nil
}

func (s *SourceSession) closeReceivers() {
	if err := s.audio.Close(); err != nil {
		s.logger.Warn("close audio receiver", "error", err)
	}
	if err := s.video.Close(); err != nil {
		s.logger.Warn("close video receiver", "error", err)
	}
}

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
	starting     bool
	startCancel  context.CancelFunc
	startDone    chan struct{}
	remoteEnded  bool
	terminating  bool
	onStarted    func()
}

func NewSourceSession(logger *slog.Logger, sourceConfig SourceConfig, entrypointID core.EntrypointID, sip SourceSIP, av SourceAV, video, audio SourceReceiver) *SourceSession {
	if logger == nil {
		logger = slog.Default()
	}
	return &SourceSession{logger: logger.With("component", "media.session", "model", sourceConfig.Model, "entrypoint_id", entrypointID), sourceConfig: sourceConfig, entrypointID: entrypointID, sip: sip, av: av, video: video, audio: audio}
}

func (s *SourceSession) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started || s.starting {
		s.mu.Unlock()
		return ErrSourceSessionStarted
	}
	if s.sourceConfig.Model == "" || s.sourceConfig.DevAddr == "" || s.entrypointID == "" || s.sip == nil || s.av == nil || s.video == nil || s.audio == nil {
		s.mu.Unlock()
		return errors.New("media: incomplete source session")
	}
	startCtx, cancel := context.WithCancel(ctx)
	s.starting = true
	s.remoteEnded = false
	s.terminating = false
	s.startCancel = cancel
	s.startDone = make(chan struct{})
	s.mu.Unlock()

	var videoStarted, audioStarted, sipStarted bool
	started := false
	defer func() {
		if !started {
			s.mu.Lock()
			hangup := sipStarted && !s.remoteEnded && !s.terminating
			if hangup {
				s.terminating = true
			}
			s.mu.Unlock()
			if hangup {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := s.sip.Hangup(cleanupCtx); err != nil {
					s.logger.WarnContext(cleanupCtx, "sip cleanup after startup failure failed", "error", err)
				}
				cleanupCancel()
			}
			if audioStarted || videoStarted {
				s.closeReceivers()
			}
		}
		var onStarted func()
		s.mu.Lock()
		s.started = started
		s.starting = false
		s.startCancel = nil
		close(s.startDone)
		s.startDone = nil
		if started {
			onStarted = s.onStarted
		}
		s.mu.Unlock()
		if onStarted != nil {
			onStarted()
		}
	}()

	s.logger.InfoContext(startCtx, "source session starting", "dev_addr", s.sourceConfig.DevAddr, "high_res_video", s.sourceConfig.HighResVideo)
	if err := s.video.Start(startCtx); err != nil {
		return fmt.Errorf("start video receiver: %w", err)
	}
	videoStarted = true
	if err := s.audio.Start(startCtx); err != nil {
		return fmt.Errorf("start audio receiver: %w", err)
	}
	audioStarted = true
	if err := s.sip.StartStream(startCtx, s.sourceConfig.DevAddr); err != nil {
		return fmt.Errorf("start outgoing sip: %w", err)
	}
	sipStarted = true
	if err := s.av.Start(startCtx, s.sourceConfig.HighResVideo, s.video, s.audio); err != nil {
		return fmt.Errorf("start av: %w", err)
	}
	if err := startCtx.Err(); err != nil {
		return fmt.Errorf("start source session: %w", err)
	}
	started = true
	s.logger.InfoContext(startCtx, "source session started")
	return nil
}

// SetStartedCallback runs once the session has completed SIP and AV activation.
func (s *SourceSession) SetStartedCallback(callback func()) {
	s.mu.Lock()
	s.onStarted = callback
	s.mu.Unlock()
}

func (s *SourceSession) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.starting {
		cancel, done := s.startCancel, s.startDone
		s.mu.Unlock()
		cancel()
		<-done
		return nil
	}
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.logger.InfoContext(ctx, "source session stopping")
	s.started = false
	hangup := !s.remoteEnded && !s.terminating
	if hangup {
		s.terminating = true
	}
	s.mu.Unlock()
	s.closeReceivers()
	if hangup {
		if err := s.sip.Hangup(ctx); err != nil {
			return fmt.Errorf("stop outgoing sip: %w", err)
		}
	}
	s.logger.InfoContext(ctx, "source session stopped")
	return nil
}

// RemoteDialogEnded releases local media after the peer terminates the SIP dialog.
// It deliberately does not send BYE because the peer has already done so.
func (s *SourceSession) RemoteDialogEnded() {
	s.mu.Lock()
	s.remoteEnded = true
	if s.starting {
		cancel := s.startCancel
		s.mu.Unlock()
		cancel()
		return
	}
	if !s.started {
		s.mu.Unlock()
		return
	}

	s.started = false
	s.mu.Unlock()
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

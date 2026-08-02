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

	// RemoteDialogEnded drops the dialog the peer has already torn down,
	// without sending a BYE back. The session calls it on exactly the paths
	// where it skips Hangup, so the SIP layer is never left holding a dialog
	// nothing will ever end. See SourceSession.RemoteDialogEnded.
	RemoteDialogEnded()
}

// AVPorts carries the loopback ports the receivers actually bound, which the
// AV request advertises to the intercom as its send destination.
type AVPorts struct {
	Video int
	Audio int
}

type SourceAV interface {
	Start(context.Context, bool, AVPorts, FlowProbe, FlowProbe) error
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

			// The startup path is the only one that knows whether the INVITE
			// had already gone out when the peer ended the dialog, so it is the
			// one that tells the SIP layer. The two flags are mutually
			// exclusive — a BYE either goes out or the peer already sent one.
			remoteEnded := sipStarted && s.remoteEnded && !s.terminating
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

			if remoteEnded {
				s.sip.RemoteDialogEnded()
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

	videoPort := s.video.Metadata().LocalPort
	audioPort := s.audio.Metadata().LocalPort

	if videoPort == 0 || audioPort == 0 {
		return fmt.Errorf("media: receiver reported an unbound port (video=%d, audio=%d)", videoPort, audioPort)
	}

	if err := s.av.Start(startCtx, s.sourceConfig.HighResVideo, AVPorts{Video: videoPort, Audio: audioPort}, s.video, s.audio); err != nil {
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

		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
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
//
// Skipping the BYE is precisely why the SIP layer has to be told. Hangup is what
// normally makes the SIP layer forget the dialog, and Close will not run it once
// remoteEnded is set. The manager behind SourceSIP is a single process-wide
// instance shared by the HTTP API and every media source, so a dialog it still
// believes is up is permanent: every later preview fails with ErrActiveDialog
// and every later inbound INVITE is answered 486 Busy Here.
//
// The SIP layer is told only where a dialog really existed and no BYE will be
// sent for it:
//
//   - starting — the INVITE may not have gone out yet, and only Start knows.
//     Cancelling the startup makes its deferred cleanup decide, and notify.
//   - not started — either no dialog was ever created, or Close already ran
//     Hangup, or this is a repeat call. Notifying here would drop a dialog this
//     session does not own, an answered inbound call among them.
//   - started — the INVITE succeeded and Close is about to skip the BYE. This is
//     the case that must notify.
//
// The SIP layer is called with s.mu released. Nothing reachable from it calls
// back into a session — the dialer runs its remote-BYE callback on a fresh
// goroutine holding no lock — so the only edge is s.mu -> the manager's lock,
// and releasing s.mu first keeps even that one from being taken.
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
	s.sip.RemoteDialogEnded()
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

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
	// StartStream dials the intercom and reports whether it created the SIP
	// dialog. It reports false when an answered inbound call is already up: no
	// INVITE goes out and the dialog belongs to that call, so this session must
	// never send a BYE for it as a side effect of a media teardown.
	StartStream(context.Context, string) (bool, error)

	// AdoptRingingCall is StartStream for a ringing preview: it never dials, and
	// reports whether the inbound call has already been answered.
	AdoptRingingCall(devAddr string) (bool, error)

	// HangupCall ends an active dialog with a BYE and leaves a ringing call
	// alone. It is the only hangup a media session may reach: rejecting a ringing
	// call with a 603 would end every fork, the indoor unit's included. See
	// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
	HangupCall(context.Context) error

	// RemoteDialogEnded drops the dialog the peer has already torn down,
	// without sending a BYE back. The session calls it on exactly the paths
	// where it skips HangupCall, so the SIP layer is never left holding a dialog
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
	StartVideo(context.Context, bool, int, FlowProbe) error
	StartAudio(context.Context, int, FlowProbe) error
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

// SourceSession owns both RTP sockets for one device source, and owns the SIP
// dialog only when it created it. A lease adopted for an answered inbound call
// borrows the call's dialog: see ownsDialog, and
// docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md.
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

	// ownsDialog records whether StartStream created the SIP dialog. False for a
	// lease adopted for an answered inbound call, whose dialog belongs to the
	// call: no teardown of this session's media may end it.
	ownsDialog bool

	// endCallOnClose lets a teardown the consumer asked for end a dialog this
	// session did not create. Nothing else may.
	endCallOnClose bool
	onStarted      func()

	// ringingPreview makes Start borrow the ringing inbound call instead of
	// dialling, and start video only. Set by PrepareRingingPreview before Start.
	ringingPreview bool

	// ringing is true while a started preview is still video-only, waiting for
	// the answer. Answered clears it once audio is flowing.
	ringing bool
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
	s.ownsDialog = false
	s.endCallOnClose = false
	s.ringing = false
	preview := s.ringingPreview
	s.startCancel = cancel
	s.startDone = make(chan struct{})
	s.mu.Unlock()

	var videoStarted, audioStarted, sipStarted bool

	started := false
	defer func() {
		if !started {
			s.mu.Lock()

			// ownsDialog is what keeps a failed startup from ending a call it
			// merely borrowed. On a lease adopted for an answered inbound call no
			// INVITE went out, so there is no dialog of ours to BYE — and the one
			// that exists is the call the user is on.
			hangup := sipStarted && s.ownsDialog && !s.remoteEnded && !s.terminating

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
				if err := s.sip.HangupCall(cleanupCtx); err != nil {
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

	// A ringing preview borrows the call: no INVITE, no dialog of its own, so no
	// teardown of this session can end the call while it rings.
	var owns bool

	answered := true

	if preview {
		var err error

		answered, err = s.sip.AdoptRingingCall(s.sourceConfig.DevAddr)
		if err != nil {
			return fmt.Errorf("adopt ringing call: %w", err)
		}
	} else {
		var err error

		owns, err = s.sip.StartStream(startCtx, s.sourceConfig.DevAddr)
		if err != nil {
			return fmt.Errorf("start outgoing sip: %w", err)
		}
	}

	sipStarted = true

	s.mu.Lock()
	s.ownsDialog = owns
	s.mu.Unlock()

	videoPort := s.video.Metadata().LocalPort
	audioPort := s.audio.Metadata().LocalPort

	if videoPort == 0 || audioPort == 0 {
		return fmt.Errorf("media: receiver reported an unbound port (video=%d, audio=%d)", videoPort, audioPort)
	}

	ports := AVPorts{Video: videoPort, Audio: audioPort}

	// The intercom has no audio before the answer, so a preview of a call that
	// is still ringing asks for video alone; Answered adds the audio later on
	// the port bound here.
	var avErr error
	if answered {
		avErr = s.av.Start(startCtx, s.sourceConfig.HighResVideo, ports, s.video, s.audio)
	} else {
		avErr = s.av.StartVideo(startCtx, s.sourceConfig.HighResVideo, ports.Video, s.video)
	}

	if avErr != nil {
		return fmt.Errorf("start av: %w", avErr)
	}

	if err := startCtx.Err(); err != nil {
		return fmt.Errorf("start source session: %w", err)
	}

	s.mu.Lock()
	s.ringing = !answered
	s.mu.Unlock()

	started = true

	s.logger.InfoContext(startCtx, "source session started", "ringing_preview", preview, "ringing", !answered)

	return nil
}

// SetStartedCallback runs once the session has completed SIP and AV activation.
func (s *SourceSession) SetStartedCallback(callback func()) {
	s.mu.Lock()
	s.onStarted = callback
	s.mu.Unlock()
}

// EndCallOnClose tells the session that the teardown about to run came from the
// consumer letting go of the lease, so the next Close must end the SIP call even
// for a dialog this session did not create.
//
// Releasing the lease ending the doorbell call is deliberate: without a WebRTC
// session there is no audio path to the user, so a call left up would leave the
// visitor talking to nobody. What must not end the call is a media teardown
// under a consumer that is still there — a foreign OpenWebNet stop frame, an
// RTP stall, a transport failure. See
// docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md.
func (s *SourceSession) EndCallOnClose() {
	s.mu.Lock()
	s.endCallOnClose = true
	s.mu.Unlock()
}

// PrepareRingingPreview makes the next Start serve a consumer that took the
// stream while the inbound call rings: it borrows the call instead of dialling
// and asks for video only. See
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
func (s *SourceSession) PrepareRingingPreview() {
	s.mu.Lock()
	s.ringingPreview = true
	s.mu.Unlock()
}

// Ringing reports whether the session is a started preview still waiting for
// the answer: video only, no audio requested yet.
func (s *SourceSession) Ringing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.ringing
}

// Answered completes a ringing preview once the call is answered: it asks the
// intercom for audio on the port bound at Start. Video, sockets and the
// consumer's session are untouched, which is what makes the answer instant.
// It is a no-op on a session that is not ringing, so a repeated call is safe.
// On failure the session stays ringing and the caller decides what to do.
func (s *SourceSession) Answered(ctx context.Context) error {
	s.mu.Lock()
	if !s.started || !s.ringing {
		s.mu.Unlock()

		return nil
	}
	s.mu.Unlock()

	if err := s.av.StartAudio(ctx, s.audio.Metadata().LocalPort, s.audio); err != nil {
		return fmt.Errorf("start av audio: %w", err)
	}

	s.mu.Lock()
	s.ringing = false
	s.mu.Unlock()

	s.logger.InfoContext(ctx, "ringing preview joined the answered call")

	return nil
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

	hangup := !s.remoteEnded && !s.terminating && (s.ownsDialog || s.endCallOnClose)
	if hangup {
		s.terminating = true
	}
	s.mu.Unlock()
	s.closeReceivers()

	if hangup {
		if err := s.sip.HangupCall(ctx); err != nil {
			return fmt.Errorf("stop outgoing sip: %w", err)
		}
	}

	s.logger.InfoContext(ctx, "source session stopped")

	return nil
}

// RemoteDialogEnded releases local media after the peer terminates the SIP dialog.
// It deliberately does not send BYE because the peer has already done so.
//
// Skipping the BYE is precisely why the SIP layer has to be told. HangupCall is what
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

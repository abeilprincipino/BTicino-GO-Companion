package media

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

var (
	ErrEntrypointSwitchBlocked = errors.New("cannot switch entrypoint while stream is active")
)

type Backend interface {
	StreamStart(ctx context.Context, devAddr string) error
	StreamStop(ctx context.Context) error
}

type readerSession struct {
	EntrypointID string
	DevAddr      string
	LastSeen     time.Time
}

type Snapshot struct {
	StreamActive     bool   `json:"stream_active"`
	ActiveEntrypoint string `json:"active_entrypoint,omitempty"`
	ManualHold       bool   `json:"manual_hold"`
	ReaderCount      int    `json:"reader_count"`
}

type Transition struct {
	Kind         string
	EntrypointID string
	DevAddr      string
	Source       string
	Reason       string
}

type TransitionSink func(Transition)

type Service struct {
	backend Backend
	logger  *log.Logger

	mu               sync.RWMutex
	streamActive     bool
	activeEntrypoint string
	activeDevAddr    string
	manualHold       bool
	readers          map[string]readerSession
	transitionSink   TransitionSink
}

func NewService(backend Backend) *Service {
	return &Service{
		backend: backend,
		readers: map[string]readerSession{},
	}
}

func (s *Service) SetLogger(logger *log.Logger) {
	s.mu.Lock()
	s.logger = logger
	s.mu.Unlock()
}

func (s *Service) SetTransitionSink(sink TransitionSink) {
	s.mu.Lock()
	s.transitionSink = sink
	s.mu.Unlock()
}

func (s *Service) StartForEntrypoint(ctx context.Context, entrypointID string, devAddr string) error {
	s.mu.Lock()
	s.logfLocked("media start request source=api entrypoint=%s devaddr=%s active=%v active_entrypoint=%s readers=%d manual_hold=%v", entrypointID, devAddr, s.streamActive, s.activeEntrypoint, len(s.readers), s.manualHold)
	if s.streamActive {
		if s.activeEntrypoint == entrypointID {
			s.manualHold = true
			s.logfLocked("media start noop source=api entrypoint=%s reason=already_active", entrypointID)
			s.mu.Unlock()
			return nil
		}
		if len(s.readers) > 0 {
			s.logfLocked("media start blocked source=api entrypoint=%s active_entrypoint=%s readers=%d", entrypointID, s.activeEntrypoint, len(s.readers))
			s.mu.Unlock()
			return ErrEntrypointSwitchBlocked
		}
	}
	s.mu.Unlock()

	if s.backend == nil {
		s.mu.Lock()
		s.streamActive = true
		s.activeEntrypoint = entrypointID
		s.activeDevAddr = devAddr
		s.manualHold = true
		s.mu.Unlock()
		s.logf("media start complete source=api entrypoint=%s devaddr=%s backend=none", entrypointID, devAddr)
		s.emitTransition(Transition{
			Kind:         "stream.started",
			EntrypointID: entrypointID,
			DevAddr:      devAddr,
			Source:       "api",
			Reason:       "manual_start",
		})
		return nil
	}

	if err := s.backend.StreamStart(ctx, devAddr); err != nil {
		s.logf("media start failed source=api entrypoint=%s devaddr=%s err=%v", entrypointID, devAddr, err)
		return err
	}

	s.mu.Lock()
	s.streamActive = true
	s.activeEntrypoint = entrypointID
	s.activeDevAddr = devAddr
	s.manualHold = true
	s.mu.Unlock()
	s.logf("media start complete source=api entrypoint=%s devaddr=%s", entrypointID, devAddr)
	s.emitTransition(Transition{
		Kind:         "stream.started",
		EntrypointID: entrypointID,
		DevAddr:      devAddr,
		Source:       "api",
		Reason:       "manual_start",
	})
	return nil
}

func (s *Service) StopForEntrypoint(ctx context.Context, _ string) error {
	s.mu.Lock()
	s.logfLocked("media stop request source=api active=%v active_entrypoint=%s readers=%d manual_hold=%v", s.streamActive, s.activeEntrypoint, len(s.readers), s.manualHold)
	s.manualHold = false
	shouldStop := s.streamActive && len(s.readers) == 0
	s.mu.Unlock()

	if !shouldStop {
		return nil
	}
	return s.stopStream(ctx, "api", "manual_stop")
}

func (s *Service) ReaderJoin(ctx context.Context, sessionID string, entrypointID string, devAddr string) error {
	now := time.Now()

	s.mu.Lock()
	s.logfLocked("media reader join request session=%s entrypoint=%s devaddr=%s active=%v active_entrypoint=%s readers=%d", sessionID, entrypointID, devAddr, s.streamActive, s.activeEntrypoint, len(s.readers))
	if s.streamActive && s.activeEntrypoint != "" && s.activeEntrypoint != entrypointID {
		s.logfLocked("media reader join blocked session=%s entrypoint=%s active_entrypoint=%s", sessionID, entrypointID, s.activeEntrypoint)
		s.mu.Unlock()
		return ErrEntrypointSwitchBlocked
	}

	s.readers[sessionID] = readerSession{
		EntrypointID: entrypointID,
		DevAddr:      devAddr,
		LastSeen:     now,
	}

	if s.streamActive {
		if s.activeEntrypoint == "" {
			s.activeEntrypoint = entrypointID
			s.activeDevAddr = devAddr
		}
		s.mu.Unlock()
		s.logf("media reader join noop session=%s entrypoint=%s reason=already_active", sessionID, entrypointID)
		return nil
	}
	s.mu.Unlock()

	if s.backend != nil {
		if err := s.backend.StreamStart(ctx, devAddr); err != nil {
			s.mu.Lock()
			delete(s.readers, sessionID)
			s.mu.Unlock()
			s.logf("media reader join failed session=%s entrypoint=%s devaddr=%s err=%v", sessionID, entrypointID, devAddr, err)
			return err
		}
	}

	s.mu.Lock()
	s.streamActive = true
	s.activeEntrypoint = entrypointID
	s.activeDevAddr = devAddr
	s.mu.Unlock()
	s.logf("media reader join complete session=%s entrypoint=%s devaddr=%s", sessionID, entrypointID, devAddr)
	s.emitTransition(Transition{
		Kind:         "stream.started",
		EntrypointID: entrypointID,
		DevAddr:      devAddr,
		Source:       "rtsp",
		Reason:       "reader_join",
	})
	return nil
}

func (s *Service) ReaderTouch(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reader, ok := s.readers[sessionID]
	if !ok {
		return
	}
	reader.LastSeen = time.Now()
	s.readers[sessionID] = reader
}

func (s *Service) ReaderLeave(ctx context.Context, sessionID string) error {
	s.mu.Lock()
	_, existed := s.readers[sessionID]
	delete(s.readers, sessionID)
	shouldStop := s.streamActive && len(s.readers) == 0 && !s.manualHold
	s.logfLocked("media reader leave session=%s existed=%v active=%v remaining_readers=%d manual_hold=%v should_stop=%v", sessionID, existed, s.streamActive, len(s.readers), s.manualHold, shouldStop)
	s.mu.Unlock()

	if !shouldStop {
		return nil
	}
	return s.stopStream(ctx, "rtsp", "reader_leave")
}

func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		StreamActive:     s.streamActive,
		ActiveEntrypoint: s.activeEntrypoint,
		ManualHold:       s.manualHold,
		ReaderCount:      len(s.readers),
	}
}

func (s *Service) stopStream(ctx context.Context, source string, reason string) error {
	s.mu.RLock()
	if !s.streamActive {
		s.mu.RUnlock()
		s.logf("media stop noop source=%s reason=%s active=false", source, reason)
		return nil
	}
	entrypointID := s.activeEntrypoint
	devAddr := s.activeDevAddr
	s.mu.RUnlock()

	if s.backend != nil {
		if err := s.backend.StreamStop(ctx); err != nil {
			s.logf("media stop failed source=%s reason=%s entrypoint=%s devaddr=%s err=%v", source, reason, entrypointID, devAddr, err)
			return err
		}
	}
	s.mu.Lock()
	s.streamActive = false
	s.activeEntrypoint = ""
	s.activeDevAddr = ""
	s.mu.Unlock()
	s.logf("media stop complete source=%s reason=%s entrypoint=%s devaddr=%s", source, reason, entrypointID, devAddr)
	s.emitTransition(Transition{
		Kind:         "stream.stopped",
		EntrypointID: entrypointID,
		DevAddr:      devAddr,
		Source:       source,
		Reason:       reason,
	})
	return nil
}

func (s *Service) logf(format string, args ...any) {
	s.mu.RLock()
	logger := s.logger
	s.mu.RUnlock()
	if logger != nil {
		logger.Printf(format, args...)
	}
}

func (s *Service) logfLocked(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}

func (s *Service) emitTransition(transition Transition) {
	s.mu.RLock()
	sink := s.transitionSink
	s.mu.RUnlock()
	if sink == nil {
		return
	}
	sink(transition)
}

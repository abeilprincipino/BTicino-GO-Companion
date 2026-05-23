package media

import (
	"context"
	"errors"
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

func (s *Service) SetTransitionSink(sink TransitionSink) {
	s.mu.Lock()
	s.transitionSink = sink
	s.mu.Unlock()
}

func (s *Service) StartForEntrypoint(ctx context.Context, entrypointID string, devAddr string) error {
	s.mu.Lock()
	if s.streamActive {
		if s.activeEntrypoint == entrypointID {
			s.manualHold = true
			s.mu.Unlock()
			return nil
		}
		if len(s.readers) > 0 {
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
		return err
	}

	s.mu.Lock()
	s.streamActive = true
	s.activeEntrypoint = entrypointID
	s.activeDevAddr = devAddr
	s.manualHold = true
	s.mu.Unlock()
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
	if s.streamActive && s.activeEntrypoint != "" && s.activeEntrypoint != entrypointID {
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
		return nil
	}
	s.mu.Unlock()

	if s.backend != nil {
		if err := s.backend.StreamStart(ctx, devAddr); err != nil {
			s.mu.Lock()
			delete(s.readers, sessionID)
			s.mu.Unlock()
			return err
		}
	}

	s.mu.Lock()
	s.streamActive = true
	s.activeEntrypoint = entrypointID
	s.activeDevAddr = devAddr
	s.mu.Unlock()
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
	delete(s.readers, sessionID)
	shouldStop := s.streamActive && len(s.readers) == 0 && !s.manualHold
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
		return nil
	}
	entrypointID := s.activeEntrypoint
	devAddr := s.activeDevAddr
	s.mu.RUnlock()

	if s.backend != nil {
		if err := s.backend.StreamStop(ctx); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.streamActive = false
	s.activeEntrypoint = ""
	s.activeDevAddr = ""
	s.mu.Unlock()
	s.emitTransition(Transition{
		Kind:         "stream.stopped",
		EntrypointID: entrypointID,
		DevAddr:      devAddr,
		Source:       source,
		Reason:       reason,
	})
	return nil
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

package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/domain/event"
)

var (
	ErrEntrypointNotFound   = errors.New("entrypoint not found")
	ErrCapabilityNotEnabled = errors.New("entrypoint capability not enabled")
)

type UnlockDriver interface {
	Unlock(ctx context.Context, devAddr string) error
}

type StreamDriver interface {
	StreamStart(ctx context.Context, devAddr string) error
	StreamStop(ctx context.Context) error
}

type Service struct {
	entrypoints map[string]entrypoint.Model
	stream      StreamDriver
	unlock      UnlockDriver
	emit        func(event.Envelope)
}

func New(entrypoints []entrypoint.Model, stream StreamDriver, unlock UnlockDriver, emit func(event.Envelope)) *Service {
	index := make(map[string]entrypoint.Model, len(entrypoints))
	for _, ep := range entrypoints {
		id := strings.TrimSpace(ep.ID)
		if id == "" {
			continue
		}
		index[id] = ep
	}
	return &Service{
		entrypoints: index,
		stream:      stream,
		unlock:      unlock,
		emit:        emit,
	}
}

func (s *Service) UnlockEntrypoint(ctx context.Context, id string) error {
	ep, err := s.entrypoint(id)
	if err != nil {
		return err
	}
	if !ep.HasUnlock {
		return fmt.Errorf("%w: unlock", ErrCapabilityNotEnabled)
	}
	if err := s.unlock.Unlock(ctx, ep.DevAddr); err != nil {
		return err
	}
	s.publish("unlock.triggered", id, map[string]any{"devaddr": ep.DevAddr})
	return nil
}

func (s *Service) StartEntrypointStream(ctx context.Context, id string) error {
	ep, err := s.entrypoint(id)
	if err != nil {
		return err
	}
	if !ep.HasStream {
		return fmt.Errorf("%w: stream", ErrCapabilityNotEnabled)
	}
	if err := s.stream.StreamStart(ctx, ep.DevAddr); err != nil {
		return err
	}
	s.publish("stream.started", id, map[string]any{"channel": "video", "devaddr": ep.DevAddr})
	return nil
}

func (s *Service) StopEntrypointStream(ctx context.Context, id string) error {
	ep, err := s.entrypoint(id)
	if err != nil {
		return err
	}
	if !ep.HasStream {
		return fmt.Errorf("%w: stream", ErrCapabilityNotEnabled)
	}
	if err := s.stream.StreamStop(ctx); err != nil {
		return err
	}
	s.publish("stream.stopped", id, map[string]any{"devaddr": ep.DevAddr})
	return nil
}

func (s *Service) entrypoint(id string) (entrypoint.Model, error) {
	ep, ok := s.entrypoints[strings.TrimSpace(id)]
	if !ok {
		return entrypoint.Model{}, ErrEntrypointNotFound
	}
	return ep, nil
}

func (s *Service) publish(kind string, entrypointID string, payload map[string]any) {
	if s.emit == nil {
		return
	}
	s.emit(event.Envelope{
		Type:         kind,
		TS:           time.Now().UTC(),
		Source:       event.SourceAPI,
		EntrypointID: entrypointID,
		Payload:      payload,
	})
}

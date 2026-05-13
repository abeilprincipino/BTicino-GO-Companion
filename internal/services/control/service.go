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
	ErrNoIncomingCall       = errors.New("no incoming call")
	ErrNoActiveCall         = errors.New("no active call")
	ErrAudioControlDisabled = errors.New("audio control unavailable")
)

type UnlockDriver interface {
	Unlock(ctx context.Context, devAddr string) error
}

type StreamDriver interface {
	StartForEntrypoint(ctx context.Context, entrypointID string, devAddr string) error
	StopForEntrypoint(ctx context.Context, entrypointID string) error
}

type CallDriver interface {
	Answer(ctx context.Context) error
	Hangup(ctx context.Context) error
}

type AudioDriver interface {
	Mute(ctx context.Context) error
	Unmute(ctx context.Context) error
}

type Service struct {
	entrypoints map[string]entrypoint.Model
	stream      StreamDriver
	unlock      UnlockDriver
	call        CallDriver
	audio       AudioDriver
	emit        func(event.Envelope)
}

func New(entrypoints []entrypoint.Model, stream StreamDriver, unlock UnlockDriver, call CallDriver, audio AudioDriver, emit func(event.Envelope)) *Service {
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
		call:        call,
		audio:       audio,
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
	s.publish(event.TypeUnlockTriggered, id, map[string]any{"devaddr": ep.DevAddr})
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
	if err := s.stream.StartForEntrypoint(ctx, ep.ID, ep.DevAddr); err != nil {
		return err
	}
	s.publish(event.TypeStreamStarted, id, map[string]any{"channel": "video", "devaddr": ep.DevAddr})
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
	if err := s.stream.StopForEntrypoint(ctx, ep.ID); err != nil {
		return err
	}
	s.publish(event.TypeStreamStopped, id, map[string]any{"devaddr": ep.DevAddr})
	return nil
}

func (s *Service) AnswerCall(ctx context.Context) error {
	if s.call == nil {
		return ErrNoIncomingCall
	}
	if err := s.call.Answer(ctx); err != nil {
		return mapCallError(err)
	}
	s.publish(event.TypeCallAnswered, "", map[string]any{"source": "api"})
	return nil
}

func (s *Service) HangupCall(ctx context.Context) error {
	if s.call == nil {
		return ErrNoActiveCall
	}
	if err := s.call.Hangup(ctx); err != nil {
		return mapCallError(err)
	}
	s.publish(event.TypeCallEnded, "", map[string]any{"source": "api", "reason": "hangup_requested"})
	return nil
}

func (s *Service) MuteAudio(ctx context.Context) error {
	if s.audio == nil {
		return ErrAudioControlDisabled
	}
	if err := s.audio.Mute(ctx); err != nil {
		return err
	}
	s.publish(event.TypeAudioMuted, "", map[string]any{"source": "api"})
	return nil
}

func (s *Service) UnmuteAudio(ctx context.Context) error {
	if s.audio == nil {
		return ErrAudioControlDisabled
	}
	if err := s.audio.Unmute(ctx); err != nil {
		return err
	}
	s.publish(event.TypeAudioUnmuted, "", map[string]any{"source": "api"})
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
		TS:           time.Now(),
		Source:       event.SourceAPI,
		EntrypointID: entrypointID,
		Payload:      payload,
	})
}

func mapCallError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(msg, "incoming"):
		return ErrNoIncomingCall
	case strings.Contains(msg, "active call"), strings.Contains(msg, "no active call"):
		return ErrNoActiveCall
	default:
		return err
	}
}

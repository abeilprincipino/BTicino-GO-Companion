package media

import (
	"context"
	"errors"
	"fmt"

	"bticino-go-companion/internal/core"
)

var (
	ErrSnapshotUnavailable = errors.New("media: snapshot is unavailable")
	ErrSnapshotNoVideo     = errors.New("media: snapshot video is unavailable")
)

type SnapshotMedia interface {
	ActiveSource(core.EntrypointID, MediaKind) (Source, bool)
	RegisterSessionConsumer(Source, SessionID, Consumer) error
	UnregisterSessionConsumer(Source, SessionID) bool
}

type OpenWebNetVideo interface {
	StartVideo(context.Context, core.EntrypointID) error
}

type SnapshotCapture interface {
	Consumer
	Wait(context.Context) ([]byte, error)
	Close() error
}

type GStreamerSnapshot interface {
	StartSnapshot(context.Context) (SnapshotCapture, error)
}

type SnapshotService struct {
	media      SnapshotMedia
	gstreamer  GStreamerSnapshot
	openwebnet OpenWebNetVideo
}

func NewSnapshotService(media SnapshotMedia, gstreamer GStreamerSnapshot, openwebnet OpenWebNetVideo) *SnapshotService {
	return &SnapshotService{media: media, gstreamer: gstreamer, openwebnet: openwebnet}
}

func (s *SnapshotService) Capture(ctx context.Context, entrypointID core.EntrypointID) ([]byte, error) {
	if s == nil || s.media == nil || s.gstreamer == nil || entrypointID == "" {
		return nil, ErrSnapshotUnavailable
	}
	capture, err := s.gstreamer.StartSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if capture == nil {
		return nil, ErrSnapshotUnavailable
	}
	defer capture.Close()

	source, ok := s.media.ActiveSource(entrypointID, MediaKindVideo)
	if !ok {
		if s.openwebnet == nil {
			return nil, ErrSnapshotNoVideo
		}
		if err := s.openwebnet.StartVideo(ctx, entrypointID); err != nil {
			return nil, err
		}
		source, ok = s.media.ActiveSource(entrypointID, MediaKindVideo)
		if !ok {
			return nil, ErrSnapshotNoVideo
		}
	}
	sessionID := SessionID(fmt.Sprintf("snapshot:%s", entrypointID))
	if err := s.media.RegisterSessionConsumer(source, sessionID, capture); err != nil {
		return nil, err
	}
	defer s.media.UnregisterSessionConsumer(source, sessionID)
	return capture.Wait(ctx)
}

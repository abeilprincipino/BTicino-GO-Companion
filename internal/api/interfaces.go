package api

import (
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"bticino-go-companion/internal/system"
	"context"
)

type EntrypointControl interface {
	Unlock(ctx context.Context, id core.EntrypointID) error
	Stream(ctx context.Context, id core.EntrypointID) error
	Snapshot(ctx context.Context, id core.EntrypointID) ([]byte, error)
}

type AudioControl interface {
	Mute(ctx context.Context) error
	Unmute(ctx context.Context) error
}

type VoicemailControl interface {
	Enable(ctx context.Context) error
	Disable(ctx context.Context) error
}

type RuntimeControl interface {
	Reboot(ctx context.Context) error
	Restart(ctx context.Context, service string) error
	Status(ctx context.Context, service string) (system.ServiceStatus, error)
}

type UpdateControl interface {
	Status(ctx context.Context) (system.UpdateStatus, error)
	Check(ctx context.Context) (system.ReleaseManifest, error)
	Apply(ctx context.Context, request system.UpdateRequest) error
	Rollback(ctx context.Context) error
}

type WebRTCControl interface {
	Offer(source media.Source, sessionID media.SessionID, offer media.SessionDescription) (media.SessionDescription, error)
	AddCandidate(sessionID media.SessionID, candidate media.ICECandidate) error
	Close(sessionID media.SessionID) error
}

type SnapshotControl interface {
	Capture(ctx context.Context, entrypointID core.EntrypointID) ([]byte, error)
}

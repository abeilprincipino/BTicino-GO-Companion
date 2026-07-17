package api

import (
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"bticino-go-companion/internal/system"
	"context"
)

type EntrypointControl interface {
	Unlock(ctx context.Context, id core.EntrypointID) error
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
	RebootAvailable() bool
	Restart(ctx context.Context, service string) error
	Status(ctx context.Context, service string) (system.ServiceStatus, error)
}

type UpdateControl interface {
	Status(ctx context.Context) (system.UpdateStatus, error)
	Check(ctx context.Context) (system.UpdateStatus, error)
	Stage(ctx context.Context) (system.UpdateStatus, error)
	Install(ctx context.Context) (system.UpdateStatus, error)
}

type WebRTCControl interface {
	Offer(ctx context.Context, sessionID, entrypointID, offerSDP string) (string, error)
	AddICECandidate(sessionID string, candidate media.ICECandidate) error
	Close(sessionID string) error
}

type SnapshotControl interface {
	Capture(ctx context.Context, entrypointID core.EntrypointID) ([]byte, error)
}

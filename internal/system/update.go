package system

import (
	"context"
	"errors"
	"time"
)

var ErrUpdateUnavailable = errors.New("system: update control is unavailable")

type Artifact struct {
	Path string
}

type VerifiedArtifactSource interface {
	Latest(context.Context) (ReleaseManifest, error)
	Artifact(context.Context, ReleaseAsset) (Artifact, error)
}

type BinaryRotation struct {
	CurrentPath  string
	PreviousPath string
}

type BinaryRotator interface {
	Rotate(context.Context, Artifact, BinaryRotation) error
}

type RollbackPlan struct {
	Service       string
	Rotation      BinaryRotation
	HealthURL     string
	HealthTimeout time.Duration
}

type PostRestartRollback interface {
	Start(context.Context, RollbackPlan) error
}

type UpdateRequest struct {
	AssetName string
	Plan      RollbackPlan
}

type Updater struct {
	source   VerifiedArtifactSource
	rotator  BinaryRotator
	rollback PostRestartRollback
}

func NewUpdater(source VerifiedArtifactSource, rotator BinaryRotator, rollback PostRestartRollback) *Updater {
	return &Updater{source: source, rotator: rotator, rollback: rollback}
}

func (u *Updater) Apply(ctx context.Context, request UpdateRequest) error {
	if u == nil || u.source == nil || u.rotator == nil || u.rollback == nil {
		return ErrUpdateUnavailable
	}
	if request.AssetName == "" || request.Plan.Service == "" || request.Plan.Rotation.CurrentPath == "" || request.Plan.Rotation.PreviousPath == "" || request.Plan.HealthURL == "" || request.Plan.HealthTimeout <= 0 {
		return ErrUpdateUnavailable
	}
	manifest, err := u.source.Latest(ctx)
	if err != nil {
		return err
	}
	asset, err := manifest.Asset(request.AssetName)
	if err != nil {
		return err
	}
	artifact, err := u.source.Artifact(ctx, asset)
	if err != nil {
		return err
	}
	if artifact.Path == "" {
		return ErrUpdateUnavailable
	}
	if err := u.rotator.Rotate(ctx, artifact, request.Plan.Rotation); err != nil {
		return err
	}
	return u.rollback.Start(ctx, request.Plan)
}

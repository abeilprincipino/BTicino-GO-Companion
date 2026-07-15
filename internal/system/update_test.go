package system

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUpdater_DelegatesBinaryRotationAndExternalRollback(t *testing.T) {
	t.Parallel()

	source := &testArtifactSource{
		manifest: ReleaseManifest{TagName: "v1", Assets: []ReleaseAsset{{Name: testAssetName}}},
		artifact: Artifact{Path: "/tmp/companion.tar.gz"},
	}
	rotator := &testRotator{}
	executor := &testPostRestartRollback{}
	updater := NewUpdater(source, rotator, executor)

	request := UpdateRequest{
		AssetName: testAssetName,
		Plan: RollbackPlan{
			Service:       testServiceName,
			Rotation:      BinaryRotation{CurrentPath: "/opt/companion", PreviousPath: "/opt/companion.previous"},
			HealthURL:     "http://127.0.0.1:8080/api/v3/health",
			HealthTimeout: time.Minute,
		},
	}
	if err := updater.Apply(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	if rotator.artifact != source.artifact || rotator.rotation != request.Plan.Rotation {
		t.Fatalf("rotation = %#v, artifact = %#v", rotator.rotation, rotator.artifact)
	}

	if executor.plan != request.Plan {
		t.Fatalf("rollback plan = %#v", executor.plan)
	}
}

func TestUpdater_DoesNotExecuteRollbackWhenRotationFails(t *testing.T) {
	t.Parallel()

	rotator := &testRotator{err: errors.New("rotation failed")}
	executor := &testPostRestartRollback{}
	updater := NewUpdater(&testArtifactSource{
		manifest: ReleaseManifest{TagName: "v1", Assets: []ReleaseAsset{{Name: testAssetName}}},
		artifact: Artifact{Path: "/tmp/companion.tar.gz"},
	}, rotator, executor)

	err := updater.Apply(context.Background(), validUpdateRequest())
	if err == nil || executor.called {
		t.Fatalf("Apply() error = %v, executor called = %t", err, executor.called)
	}
}

func TestUpdater_RejectsIncompleteRequest(t *testing.T) {
	t.Parallel()

	updater := NewUpdater(&testArtifactSource{}, &testRotator{}, &testPostRestartRollback{})
	if err := updater.Apply(context.Background(), UpdateRequest{}); !errors.Is(err, ErrUpdateUnavailable) {
		t.Fatalf("Apply() error = %v", err)
	}
}

func validUpdateRequest() UpdateRequest {
	return UpdateRequest{
		AssetName: testAssetName,
		Plan: RollbackPlan{
			Service:       testServiceName,
			Rotation:      BinaryRotation{CurrentPath: "/opt/companion", PreviousPath: "/opt/companion.previous"},
			HealthURL:     "http://127.0.0.1:8080/api/v3/health",
			HealthTimeout: time.Minute,
		},
	}
}

type testArtifactSource struct {
	manifest ReleaseManifest
	artifact Artifact
}

func (s *testArtifactSource) Latest(context.Context) (ReleaseManifest, error) {
	return s.manifest, nil
}

func (s *testArtifactSource) Artifact(context.Context, ReleaseAsset) (Artifact, error) {
	return s.artifact, nil
}

type testRotator struct {
	artifact Artifact
	rotation BinaryRotation
	err      error
}

func (r *testRotator) Rotate(_ context.Context, artifact Artifact, rotation BinaryRotation) error {
	r.artifact = artifact
	r.rotation = rotation

	return r.err
}

type testPostRestartRollback struct {
	plan   RollbackPlan
	called bool
}

func (e *testPostRestartRollback) Start(_ context.Context, plan RollbackPlan) error {
	e.plan = plan
	e.called = true

	return nil
}

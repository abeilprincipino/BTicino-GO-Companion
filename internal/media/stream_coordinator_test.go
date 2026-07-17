package media

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"testing"
	"time"
)

func TestStreamCoordinatorConfirmsReservedStart(t *testing.T) {
	c := NewStreamCoordinator(nil, testManagedSourceFactory())
	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}
	c.ObserveControlTrack(true)
	c.ObserveControlTrack(false)
	snapshot := c.Snapshot()
	if snapshot.Owner != StreamOwnerCompanion || !snapshot.Video.Requested || !snapshot.Audio.Requested {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	c.ObserveRTP(lease, true, RTPMetadata{PacketCount: 1, LastPacketAt: time.Now(), SSRC: 42})
	if !c.Snapshot().Video.Flowing(time.Now(), time.Second) {
		t.Fatal("video is not flowing")
	}
}

func TestStreamCoordinatorRejectsExternalStream(t *testing.T) {
	c := NewStreamCoordinator(nil, testManagedSourceFactory())
	c.ObserveControlTrack(true)
	_, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if !errors.Is(err, ErrExternalStream) {
		t.Fatalf("reserve = %v, want external stream error", err)
	}
}

func TestStreamCoordinatorRemoteBYEReleasesLeaseOnce(t *testing.T) {
	source := &remoteBYESource{}
	c := NewStreamCoordinator(nil, func(_ config.Entrypoint, events SourceEvents) (ManagedSource, func(), error) {
		source.callback = events.RemoteBYE
		return source, nil, nil
	})
	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}
	source.remote()
	if snapshot := c.Snapshot(); snapshot.Owner != StreamOwnerIdle {
		t.Fatalf("snapshot after remote bye = %#v", snapshot)
	}
	if source.closes != 1 || c.Release(lease) {
		t.Fatalf("source closes=%d, second release=%t", source.closes, c.Release(lease))
	}
}

func TestStreamCoordinatorReleasesStaleRequestedTracks(t *testing.T) {
	source := &remoteBYESource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) { return source, nil, nil })
	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}
	c.ObserveControlTrack(true)
	c.ObserveControlTrack(false)
	if !c.Reconcile(lease, time.Now().Add(trackFlowTimeout+time.Second)) {
		t.Fatal("stale tracks did not stop the source")
	}
	if source.closes != 1 || c.Snapshot().Owner != StreamOwnerIdle {
		t.Fatalf("source closes=%d snapshot=%#v", source.closes, c.Snapshot())
	}
}

func testManagedSourceFactory() ManagedSourceFactory {
	return func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return &fakeManagedSource{}, nil, nil
	}
}

type remoteBYESource struct {
	closes int
	callback func()
}

func (*remoteBYESource) Start(context.Context) error { return nil }
func (s *remoteBYESource) Close(context.Context) error { s.closes++; return nil }
func (s *remoteBYESource) remote() { s.callback() }

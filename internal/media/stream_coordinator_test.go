package media

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestStreamCoordinatorConfirmsReservedStart(t *testing.T) {
	c := NewStreamCoordinator(nil, testManagedSourceFactory())

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	beginControlAttempt(c)
	c.ObserveControlTrack(true)
	c.ObserveControlTrack(false)
	endControlAttempt(c)

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

func TestStreamCoordinatorStartupUsesLifecycleContext(t *testing.T) {
	source := &contextManagedSource{started: make(chan context.Context, 1)}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	lifecycleCtx := t.Context()

	startupCtx, startupCancel := context.WithTimeout(context.Background(), time.Second)
	defer startupCancel()

	lease, err := c.AcquireWithStartup(lifecycleCtx, startupCtx, config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if startCtx := <-source.started; startCtx.Err() != nil {
		t.Fatalf("source start context ended: %v", startCtx.Err())
	}

	if !c.Release(lease) {
		t.Fatal("release source lease")
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

	beginControlAttempt(c)
	c.ObserveControlTrack(true)
	c.ObserveControlTrack(false)
	endControlAttempt(c)

	if !c.Reconcile(lease, time.Now().Add(trackFlowTimeout+time.Second)) {
		t.Fatal("stale tracks did not stop the source")
	}

	if source.closes != 1 || c.Snapshot().Owner != StreamOwnerIdle {
		t.Fatalf("source closes=%d snapshot=%#v", source.closes, c.Snapshot())
	}
}

func TestStreamCoordinatorIgnoresControlStopWithoutCurrentAttemptStart(t *testing.T) {
	source := &remoteBYESource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) { return source, nil, nil })

	first, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	beginControlAttempt(c)
	c.ObserveControlTrack(true)
	c.ObserveControlTrack(false)
	endControlAttempt(c)

	if !c.Release(first) {
		t.Fatal("release first lease")
	}

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	// A replayed start from the prior lease cannot claim the new lease.
	c.ObserveControlTrack(true)
	c.ObserveControlTrack(false)

	if snapshot := c.Snapshot(); snapshot.Video.Requested || snapshot.Audio.Requested {
		t.Fatalf("stale start altered current attempt: %#v", snapshot)
	}

	// A delayed stop from a prior multicast observation must not close this lease.
	c.ObserveControlStop()

	if source.closes != 1 || c.Snapshot().Owner != StreamOwnerCompanion {
		t.Fatalf("stale stop closed current source: closes=%d snapshot=%#v", source.closes, c.Snapshot())
	}

	beginControlAttempt(c)
	c.ObserveControlTrack(true)
	c.ObserveControlTrack(false)
	endControlAttempt(c)
	c.ObserveControlStop()

	if source.closes != 2 || c.Snapshot().Owner != StreamOwnerIdle {
		t.Fatalf("matched stop did not close source: closes=%d snapshot=%#v", source.closes, c.Snapshot())
	}

	if c.Release(lease) {
		t.Fatal("released an already stopped lease")
	}
}

func TestStreamCoordinatorWriteBackchannelRTP(t *testing.T) {
	source := &backchannelManagedSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	packet := testRTPPacket(1)
	if err := c.WriteBackchannelRTP(lease, packet); err != nil {
		t.Fatalf("WriteBackchannelRTP() error = %v", err)
	}

	if source.packet != packet {
		t.Fatal("backchannel did not receive packet")
	}
}

func TestStreamCoordinatorWriteBackchannelRTPRejectsUnavailableLease(t *testing.T) {
	source := &backchannelManagedSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		lease *StreamLease
		state func()
	}{
		{name: "nil lease", lease: nil},
		{name: "stale lease", lease: &StreamLease{id: lease.id + 1}},
		{
			name:  "starting lease",
			lease: lease,
			state: func() {
				c.mu.Lock()
				c.starting = true
				c.mu.Unlock()
			},
		},
		{
			name:  "stopping lease",
			lease: lease,
			state: func() {
				c.mu.Lock()
				c.stopping = true
				c.mu.Unlock()
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c.mu.Lock()
			c.starting = false
			c.stopping = false
			c.mu.Unlock()

			if test.state != nil {
				test.state()
			}

			if err := c.WriteBackchannelRTP(test.lease, testRTPPacket(1)); !errors.Is(err, ErrBackchannelUnavailable) {
				t.Fatalf("WriteBackchannelRTP() error = %v, want ErrBackchannelUnavailable", err)
			}

			if source.packet != nil {
				t.Fatal("backchannel received packet for unavailable lease")
			}
		})
	}
}

func TestStreamCoordinatorWriteBackchannelRTPRejectsSourceWithoutBackchannel(t *testing.T) {
	c := NewStreamCoordinator(nil, testManagedSourceFactory())

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.WriteBackchannelRTP(lease, testRTPPacket(1)); !errors.Is(err, ErrBackchannelUnavailable) {
		t.Fatalf("WriteBackchannelRTP() error = %v, want ErrBackchannelUnavailable", err)
	}
}

func beginControlAttempt(c *StreamCoordinator) {
	c.mu.Lock()
	c.starting = true
	c.mu.Unlock()
}

func endControlAttempt(c *StreamCoordinator) {
	c.mu.Lock()
	c.starting = false
	c.mu.Unlock()
}

func testManagedSourceFactory() ManagedSourceFactory {
	return func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return &fakeManagedSource{}, nil, nil
	}
}

type remoteBYESource struct {
	closes   int
	callback func()
}

func (*remoteBYESource) Start(context.Context) error   { return nil }
func (s *remoteBYESource) Close(context.Context) error { s.closes++; return nil }
func (s *remoteBYESource) remote()                     { s.callback() }

type backchannelManagedSource struct {
	packet *rtp.Packet
}

type contextManagedSource struct {
	started chan context.Context
}

func (s *contextManagedSource) Start(ctx context.Context) error {
	s.started <- ctx
	return nil
}

func (*contextManagedSource) Close(context.Context) error { return nil }

func (*backchannelManagedSource) Start(context.Context) error { return nil }
func (*backchannelManagedSource) Close(context.Context) error { return nil }
func (s *backchannelManagedSource) WriteBackchannelRTP(packet *rtp.Packet) error {
	s.packet = packet
	return nil
}

func TestStreamCoordinatorAdoptsExternalStreamForAnsweredCall(t *testing.T) {
	c := NewStreamCoordinator(nil, testManagedSourceFactory())
	c.SetAnsweredCallProbe(func() bool { return true })

	// The intercom starts its own AV while the call rings, which is what marks
	// the stream external before anyone asks for a lease.
	c.ObserveControlTrack(true)

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatalf("Acquire() error = %v, want nil", err)
	}

	if owner := c.Snapshot().Owner; owner != StreamOwnerCompanion {
		t.Fatalf("owner = %q, want %q", owner, StreamOwnerCompanion)
	}

	if !c.Release(lease) {
		t.Fatal("release source lease")
	}
}

func TestStreamCoordinatorRejectsExternalStreamWithoutAnsweredCall(t *testing.T) {
	c := NewStreamCoordinator(nil, testManagedSourceFactory())
	c.SetAnsweredCallProbe(func() bool { return false })
	c.ObserveControlTrack(true)

	_, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if !errors.Is(err, ErrExternalStream) {
		t.Fatalf("Acquire() error = %v, want ErrExternalStream", err)
	}
}

func TestStreamCoordinatorAnsweredCallDoesNotOverrideABusyLease(t *testing.T) {
	c := NewStreamCoordinator(nil, testManagedSourceFactory())
	c.SetAnsweredCallProbe(func() bool { return true })

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if !errors.Is(err, ErrStreamBusy) {
		t.Fatalf("second Acquire() error = %v, want ErrStreamBusy — the lease stays exclusive", err)
	}

	if !c.Release(lease) {
		t.Fatal("release source lease")
	}
}

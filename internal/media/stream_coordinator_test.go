package media

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"sync"
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

type callEndingSource struct {
	closes       int
	endCallCalls int
}

func (*callEndingSource) Start(context.Context) error   { return nil }
func (s *callEndingSource) Close(context.Context) error { s.closes++; return nil }
func (s *callEndingSource) EndCallOnClose()             { s.endCallCalls++ }

func TestStreamCoordinatorConsumerReleaseEndsTheCall(t *testing.T) {
	source := &callEndingSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if !c.Release(lease) {
		t.Fatal("release source lease")
	}

	if source.endCallCalls != 1 {
		t.Fatalf("EndCallOnClose calls = %d, want 1", source.endCallCalls)
	}
}

func TestStreamCoordinatorOpenWebNetStopDoesNotEndTheCall(t *testing.T) {
	source := &callEndingSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	beginControlAttempt(c)
	c.ObserveControlTrack(true)
	endControlAttempt(c)
	c.ObserveControlStop()

	if source.closes != 1 {
		t.Fatalf("source closes = %d, want 1 — the media teardown itself is unchanged", source.closes)
	}

	if source.endCallCalls != 0 {
		t.Fatalf("EndCallOnClose calls = %d, want 0 — a foreign stop frame must not end the call", source.endCallCalls)
	}

	if c.Release(lease) {
		t.Fatal("released an already stopped lease")
	}
}

func TestStreamCoordinatorStalledTracksDoNotEndTheCall(t *testing.T) {
	source := &callEndingSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	beginControlAttempt(c)
	c.ObserveControlTrack(true)
	c.ObserveControlTrack(false)
	endControlAttempt(c)

	if !c.Reconcile(lease, time.Now().Add(2*trackFlowTimeout)) {
		t.Fatal("Reconcile() = false, want true — both requested tracks are long past the flow timeout")
	}

	if source.endCallCalls != 0 {
		t.Fatalf("EndCallOnClose calls = %d, want 0 — an RTP stall must not end the call", source.endCallCalls)
	}
}

// ringingPreviewSource is a managed source that can serve a ringing preview.
// startGate, when set, holds Start until closed; started is signalled when
// Start begins. answeredCh receives one value per Answered call.
type ringingPreviewSource struct {
	mu           sync.Mutex
	prepared     int
	answered     int
	closes       int
	endCallCalls int
	answerErr    error
	packet       *rtp.Packet

	startGate  chan struct{}
	started    chan struct{}
	answeredCh chan struct{}
}

func newRingingPreviewSource() *ringingPreviewSource {
	return &ringingPreviewSource{started: make(chan struct{}, 1), answeredCh: make(chan struct{}, 4)}
}

func (s *ringingPreviewSource) Start(context.Context) error {
	s.started <- struct{}{}
	if s.startGate != nil {
		<-s.startGate
	}

	return nil
}

func (s *ringingPreviewSource) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closes++

	return nil
}

func (s *ringingPreviewSource) EndCallOnClose() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.endCallCalls++
}

func (s *ringingPreviewSource) PrepareRingingPreview() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.prepared++
}

func (s *ringingPreviewSource) Answered(context.Context) error {
	s.mu.Lock()
	s.answered++
	err := s.answerErr
	s.mu.Unlock()

	s.answeredCh <- struct{}{}

	return err
}

func (s *ringingPreviewSource) WriteBackchannelRTP(packet *rtp.Packet) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.packet = packet

	return nil
}

func (s *ringingPreviewSource) counts() (prepared, answered, closes, endCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.prepared, s.answered, s.closes, s.endCallCalls
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func ringingCoordinator(source ManagedSource, ringing *bool) *StreamCoordinator {
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})
	c.SetRingingCallProbe(func() bool { return *ringing })

	return c
}

var testEntrypoint = config.Entrypoint{ID: "main", DevAddr: "20"}

func TestStreamCoordinatorRefusesAnOrdinaryLeaseWhileRinging(t *testing.T) {
	ringing := true
	c := ringingCoordinator(&fakeManagedSource{}, &ringing)

	if _, err := c.Acquire(context.Background(), testEntrypoint, SourceEvents{}); !errors.Is(err, ErrRingingCall) {
		t.Fatalf("Acquire() with an idle stream error = %v, want ErrRingingCall", err)
	}

	// The intercom's own AV for the ring marks the stream external; the refusal
	// must not depend on that.
	c.ObserveControlTrack(true)

	if _, err := c.Acquire(context.Background(), testEntrypoint, SourceEvents{}); !errors.Is(err, ErrRingingCall) {
		t.Fatalf("Acquire() with an external stream error = %v, want ErrRingingCall", err)
	}
}

func TestStreamCoordinatorRingingPreviewAdoptsTheExternalStream(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)
	c.ObserveControlTrack(true)

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatalf("AcquireRingingPreview() error = %v", err)
	}

	if prepared, _, _, _ := source.counts(); prepared != 1 {
		t.Fatalf("PrepareRingingPreview calls = %d, want 1", prepared)
	}

	if owner := c.Snapshot().Owner; owner != StreamOwnerCompanion {
		t.Fatalf("owner = %q, want %q", owner, StreamOwnerCompanion)
	}

	c.Release(lease)
}

func TestStreamCoordinatorRingingPreviewNeedsACapableSource(t *testing.T) {
	ringing := true
	c := ringingCoordinator(&fakeManagedSource{}, &ringing)

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{}); !errors.Is(err, ErrStreamBusy) {
		t.Fatalf("AcquireRingingPreview() error = %v, want ErrStreamBusy", err)
	}

	ringing = false

	lease, err := c.Acquire(context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatalf("Acquire() after the refused preview error = %v, want the lease freed", err)
	}

	c.Release(lease)
}

func TestStreamCoordinatorPreviewOutsideTheRingIsAnOrdinaryLease(t *testing.T) {
	ringing := false
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if prepared, _, _, _ := source.counts(); prepared != 0 {
		t.Fatalf("PrepareRingingPreview calls = %d, want 0", prepared)
	}

	c.StopRinging()

	if _, _, closes, _ := source.counts(); closes != 0 {
		t.Fatalf("closes = %d, want 0 — StopRinging must leave an ordinary lease alone", closes)
	}

	c.Release(lease)
}

func TestStreamCoordinatorStopRingingEndsThePreviewWithoutEndingTheCall(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)

	var failed error

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
		Failed: func(err error) { failed = err },
	}); err != nil {
		t.Fatal(err)
	}

	c.StopRinging()

	if _, _, closes, endCalls := source.counts(); closes != 1 || endCalls != 0 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 0", closes, endCalls)
	}

	if !errors.Is(failed, ErrRingingEnded) {
		t.Fatalf("consumer told %v, want ErrRingingEnded", failed)
	}

	if owner := c.Snapshot().Owner; owner != StreamOwnerIdle {
		t.Fatalf("owner = %q, want idle", owner)
	}
}

func TestStreamCoordinatorPromoteRingingAnswersTheSourceOnce(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	ringing = false
	c.PromoteRinging()
	waitSignal(t, source.answeredCh, "Answered")

	c.PromoteRinging()
	c.StopRinging()

	if _, answered, closes, _ := source.counts(); answered != 1 || closes != 0 {
		t.Fatalf("answered=%d closes=%d, want 1 and 0 — a promoted lease is no longer a preview", answered, closes)
	}

	c.Release(lease)
}

func TestStreamCoordinatorPromotionDuringStartupRunsOnceTheSourceIsUp(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	source.startGate = make(chan struct{})
	c := ringingCoordinator(source, &ringing)

	acquired := make(chan error, 1)
	leaseCh := make(chan *StreamLease, 1)
	go func() {
		l, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
		leaseCh <- l
		acquired <- err
	}()

	waitSignal(t, source.started, "Start")
	c.PromoteRinging()

	if _, answered, _, _ := source.counts(); answered != 0 {
		t.Fatalf("answered = %d before the source is up, want 0", answered)
	}

	close(source.startGate)

	if err := <-acquired; err != nil {
		t.Fatal(err)
	}

	waitSignal(t, source.answeredCh, "Answered after startup")

	c.Release(<-leaseCh)
}

func TestStreamCoordinatorPromotionFailureStopsTheLeaseWithoutEndingTheCall(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	source.answerErr = errors.New("audio nack")
	c := ringingCoordinator(source, &ringing)

	failed := make(chan error, 1)

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
		Failed: func(err error) { failed <- err },
	}); err != nil {
		t.Fatal(err)
	}

	c.PromoteRinging()

	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("consumer told nil, want the promotion error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer was never told the promotion failed")
	}

	if _, _, closes, endCalls := source.counts(); closes != 1 || endCalls != 0 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 0 — the answered call must survive", closes, endCalls)
	}
}

func TestStreamCoordinatorBlocksTheBackchannelWhileRinging(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.WriteBackchannelRTP(lease, testRTPPacket(1)); !errors.Is(err, ErrBackchannelUnavailable) {
		t.Fatalf("WriteBackchannelRTP() while ringing error = %v, want ErrBackchannelUnavailable", err)
	}

	ringing = false
	c.PromoteRinging()
	waitSignal(t, source.answeredCh, "Answered")

	if err := c.WriteBackchannelRTP(lease, testRTPPacket(2)); err != nil {
		t.Fatalf("WriteBackchannelRTP() after the answer error = %v", err)
	}

	c.Release(lease)
}

// blockingAnswerSource holds Answered until its context ends, as the real
// source does while its audio request retries.
type blockingAnswerSource struct {
	*ringingPreviewSource
	answerStarted chan struct{}
	answerDone    chan error
}

func (s *blockingAnswerSource) Answered(ctx context.Context) error {
	s.answerStarted <- struct{}{}
	<-ctx.Done()
	s.answerDone <- ctx.Err()

	return ctx.Err()
}

func TestStreamCoordinatorReleaseCancelsAPromotionInFlight(t *testing.T) {
	ringing := true
	source := &blockingAnswerSource{
		ringingPreviewSource: newRingingPreviewSource(),
		answerStarted:        make(chan struct{}, 1),
		answerDone:           make(chan error, 1),
	}
	c := ringingCoordinator(source, &ringing)

	var (
		mu          sync.Mutex
		failedCalls int
	)

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
		Failed: func(error) {
			mu.Lock()
			failedCalls++
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	c.PromoteRinging()
	waitSignal(t, source.answerStarted, "Answered to start")

	if !c.Release(lease) {
		t.Fatal("Release() = false")
	}

	select {
	case err := <-source.answerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Answered ended with %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Answered kept running after the lease was released")
	}

	// promote decides on the failure just after Answered returns; give it the
	// chance to wrongly report one.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if failedCalls != 0 {
		t.Fatalf("consumer Failed called %d times, want 0 — the teardown is not a promotion failure", failedCalls)
	}
}

func TestStreamCoordinatorWatchdogStopsAPromotedPreviewWithoutEndingTheCall(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	// The video request is the only one seen while the lease was starting.
	beginControlAttempt(c)
	c.ObserveControlTrack(true)
	endControlAttempt(c)

	c.PromoteRinging()
	waitSignal(t, source.answeredCh, "Answered")

	if !c.Reconcile(lease, time.Now().Add(trackFlowTimeout+time.Second)) {
		t.Fatal("Reconcile() = false, want true — both tracks are requested and silent past the flow timeout")
	}

	if _, _, closes, endCalls := source.counts(); closes != 1 || endCalls != 0 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 0 — a stall must not end the call", closes, endCalls)
	}
}

func TestStreamCoordinatorLeaseBornInTheAnswerWindowIsPromoted(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)
	c.SetAnsweredCallProbe(func() bool { return true })

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	// No PromoteRinging: the CallAnswered event was drained before the lease
	// existed, so only finishStart can notice the answer.
	waitSignal(t, source.answeredCh, "Answered without a PromoteRinging")

	if err := c.WriteBackchannelRTP(lease, testRTPPacket(1)); err != nil {
		t.Fatalf("WriteBackchannelRTP() error = %v, want the microphone open", err)
	}

	c.PromoteRinging()

	// Answered would signal again from its goroutine; allow it to.
	time.Sleep(100 * time.Millisecond)

	if _, answered, _, _ := source.counts(); answered != 1 {
		t.Fatalf("answered = %d, want 1", answered)
	}

	c.Release(lease)
}

func TestStreamCoordinatorControlStopTellsTheConsumerItsRingingPreviewIsLost(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)

	var (
		failures []error
		mu       sync.Mutex
	)

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
		Failed: func(err error) {
			mu.Lock()
			defer mu.Unlock()

			failures = append(failures, err)
		},
	}); err != nil {
		t.Fatal(err)
	}

	// The start frame ObserveControlStop matches against must have been seen
	// while the source attempt was starting.
	beginControlAttempt(c)
	c.ObserveControlTrack(true)
	endControlAttempt(c)

	c.ObserveControlStop()

	mu.Lock()
	defer mu.Unlock()

	if len(failures) != 1 || !errors.Is(failures[0], ErrRingingPreviewLost) {
		t.Fatalf("consumer told %v, want exactly one ErrRingingPreviewLost", failures)
	}

	if _, _, closes, endCalls := source.counts(); closes != 1 || endCalls != 0 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 0 — a lost preview must not end the call", closes, endCalls)
	}

	if owner := c.Snapshot().Owner; owner != StreamOwnerIdle {
		t.Fatalf("owner = %q, want idle", owner)
	}
}

func TestStreamCoordinatorControlStopDoesNotNotifyAnOrdinaryLeaseConsumer(t *testing.T) {
	source := &callEndingSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	failed := 0

	if _, err := c.Acquire(context.Background(), testEntrypoint, SourceEvents{
		Failed: func(error) { failed++ },
	}); err != nil {
		t.Fatal(err)
	}

	beginControlAttempt(c)
	c.ObserveControlTrack(true)
	endControlAttempt(c)
	c.ObserveControlStop()

	if source.closes != 1 {
		t.Fatalf("source closes = %d, want 1 — the stop must have run", source.closes)
	}

	if failed != 0 {
		t.Fatalf("consumer Failed called %d times, want 0 — only ringing previews are announced", failed)
	}
}

// sharedFactory builds one source per stream and records the wrapped events the
// coordinator hands it, so a test can push media into the stream.
type sharedFactory struct {
	mu      sync.Mutex
	sources []ManagedSource
	events  []SourceEvents
	build   func() ManagedSource
}

func (f *sharedFactory) factory() ManagedSourceFactory {
	return func(_ config.Entrypoint, events SourceEvents) (ManagedSource, func(), error) {
		f.mu.Lock()
		defer f.mu.Unlock()

		source := f.build()
		f.sources = append(f.sources, source)
		f.events = append(f.events, events)

		return source, nil, nil
	}
}

func (f *sharedFactory) created() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.sources)
}

func (f *sharedFactory) streamEvents(i int) SourceEvents {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.events[i]
}

func TestStreamCoordinatorShareableAcquireJoinsTheRunningStream(t *testing.T) {
	shared := &sharedFactory{build: func() ManagedSource { return &callEndingSource{} }}
	c := NewStreamCoordinator(nil, shared.factory())

	var first, second []uint16

	firstLease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
		VideoRTP: func(packet *rtp.Packet) { first = append(first, packet.SequenceNumber) },
	})
	if err != nil {
		t.Fatal(err)
	}

	secondLease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
		VideoRTP: func(packet *rtp.Packet) { second = append(second, packet.SequenceNumber) },
	})
	if err != nil {
		t.Fatalf("second AcquireRingingPreview() error = %v, want a join", err)
	}

	if shared.created() != 1 {
		t.Fatalf("sources created = %d, want 1", shared.created())
	}

	if firstLease.id != secondLease.id || firstLease.consumer == secondLease.consumer {
		t.Fatalf("leases = %#v and %#v, want one stream and two consumers", firstLease, secondLease)
	}

	shared.streamEvents(0).VideoRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: 7}, Payload: []byte{0x41}})

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("deliveries first=%v second=%v, want one each", first, second)
	}
}

func TestStreamCoordinatorExclusiveLeasesNeverShare(t *testing.T) {
	c := NewStreamCoordinator(nil, testManagedSourceFactory())

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Acquire(context.Background(), testEntrypoint, SourceEvents{}); !errors.Is(err, ErrStreamBusy) {
		t.Fatalf("Acquire() on a shared stream error = %v, want ErrStreamBusy", err)
	}

	c.Release(lease)

	exclusive, err := c.Acquire(context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{}); !errors.Is(err, ErrStreamBusy) {
		t.Fatalf("AcquireRingingPreview() on an exclusive stream error = %v, want ErrStreamBusy", err)
	}

	c.Release(exclusive)
}

func TestStreamCoordinatorOnlyTheLastReleaseEndsTheCall(t *testing.T) {
	source := &callEndingSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	first, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	second, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if !c.Release(first) {
		t.Fatal("Release(first) = false")
	}

	if source.closes != 0 || source.endCallCalls != 0 {
		t.Fatalf("after a middle release closes=%d endCalls=%d, want 0 and 0", source.closes, source.endCallCalls)
	}

	if c.Release(first) {
		t.Fatal("Release(first) twice = true, want a no-op")
	}

	if !c.Release(second) {
		t.Fatal("Release(second) = false")
	}

	if source.closes != 1 || source.endCallCalls != 1 {
		t.Fatalf("after the last release closes=%d endCalls=%d, want 1 and 1", source.closes, source.endCallCalls)
	}
}

func TestStreamCoordinatorLeaveNeverEndsTheCall(t *testing.T) {
	source := &callEndingSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if !c.Leave(lease) {
		t.Fatal("Leave() = false")
	}

	if source.closes != 1 || source.endCallCalls != 0 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 0", source.closes, source.endCallCalls)
	}

	if c.Snapshot().Owner != StreamOwnerIdle {
		t.Fatalf("owner = %q, want idle", c.Snapshot().Owner)
	}
}

func TestStreamCoordinatorJoinWaitsForTheStartup(t *testing.T) {
	source := newRingingPreviewSource()
	source.startGate = make(chan struct{})
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	type result struct {
		lease *StreamLease
		err   error
	}

	firstDone, secondDone := make(chan result, 1), make(chan result, 1)

	go func() {
		lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
		firstDone <- result{lease, err}
	}()

	waitSignal(t, source.started, "source Start")

	go func() {
		lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
		secondDone <- result{lease, err}
	}()

	select {
	case <-secondDone:
		t.Fatal("the joiner returned before the stream had started")
	case <-time.After(50 * time.Millisecond):
	}

	close(source.startGate)

	first, second := <-firstDone, <-secondDone
	if first.err != nil || second.err != nil {
		t.Fatalf("errors first=%v second=%v", first.err, second.err)
	}

	if first.lease.id != second.lease.id {
		t.Fatalf("leases on different streams: %#v and %#v", first.lease, second.lease)
	}
}

type failingGatedSource struct {
	started chan struct{}
	gate    chan struct{}
}

func (s *failingGatedSource) Start(context.Context) error {
	s.started <- struct{}{}
	<-s.gate

	return errors.New("intercom refused")
}

func (*failingGatedSource) Close(context.Context) error { return nil }

func TestStreamCoordinatorJoinFailsWithTheStartup(t *testing.T) {
	source := &failingGatedSource{started: make(chan struct{}, 1), gate: make(chan struct{})}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	firstDone, secondDone := make(chan error, 1), make(chan error, 1)

	go func() {
		_, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
		firstDone <- err
	}()

	<-source.started

	go func() {
		_, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
		secondDone <- err
	}()

	time.Sleep(20 * time.Millisecond)
	close(source.gate)

	if err := <-firstDone; err == nil {
		t.Fatal("creator error = nil, want the startup failure")
	}

	if err := <-secondDone; !errors.Is(err, ErrStreamStopped) {
		t.Fatalf("joiner error = %v, want ErrStreamStopped", err)
	}
}

func TestStreamCoordinatorStopRingingTellsEveryConsumer(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)

	var told [2]error

	for i := range told {
		if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
			Failed: func(err error) { told[i] = err },
		}); err != nil {
			t.Fatal(err)
		}
	}

	c.StopRinging()

	for i, err := range told {
		if !errors.Is(err, ErrRingingEnded) {
			t.Fatalf("consumer %d told %v, want ErrRingingEnded", i, err)
		}
	}
}

func TestStreamCoordinatorRemoteBYEReachesEveryConsumer(t *testing.T) {
	shared := &sharedFactory{build: func() ManagedSource { return &fakeManagedSource{} }}
	c := NewStreamCoordinator(nil, shared.factory())

	byes := 0

	for range 2 {
		if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
			RemoteBYE: func() { byes++ },
		}); err != nil {
			t.Fatal(err)
		}
	}

	shared.streamEvents(0).RemoteBYE()

	if byes != 2 {
		t.Fatalf("RemoteBYE delivered %d times, want 2", byes)
	}

	if c.Snapshot().Owner != StreamOwnerIdle {
		t.Fatalf("owner = %q, want idle", c.Snapshot().Owner)
	}
}

func TestStreamCoordinatorJoinerGetsTheCachedKeyframe(t *testing.T) {
	shared := &sharedFactory{build: func() ManagedSource { return &fakeManagedSource{} }}
	c := NewStreamCoordinator(nil, shared.factory())

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{}); err != nil {
		t.Fatal(err)
	}

	shared.streamEvents(0).VideoRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: 9, Timestamp: 100}, Payload: []byte{0x65, 1}})

	var replayed []uint16

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
		VideoRTP: func(packet *rtp.Packet) { replayed = append(replayed, packet.SequenceNumber) },
	}); err != nil {
		t.Fatal(err)
	}

	if len(replayed) != 1 || replayed[0] != 9 {
		t.Fatalf("joiner got %v, want the cached IDR [9]", replayed)
	}
}

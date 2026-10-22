package media

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/rtp"
)

var (
	ErrStreamBusy             = errors.New("media: intercom stream is busy")
	ErrExternalStream         = errors.New("media: intercom stream is active externally")
	ErrBackchannelUnavailable = errors.New("media: backchannel is unavailable")
	ErrRingingCall            = errors.New("media: intercom call is ringing")
	ErrRingingEnded           = errors.New("media: ringing call ended")
	ErrRingingPreviewLost     = errors.New("media: ringing preview lost")
	ErrStreamStopped          = errors.New("media: stream stopped")
)

const (
	trackFlowTimeout = 5 * time.Second
	watchInterval    = time.Second

	// promotionTimeout bounds the audio request that turns a ringing preview
	// into the answered call: three AV attempts of up to 2 s reply plus 1.2 s
	// flow wait each, with margin.
	promotionTimeout = 15 * time.Second
)

// stopCause says why a lease is being torn down, and whether that teardown may
// end the SIP call behind it.
//
// Only a consumer letting go of the lease ends the call: without a WebRTC
// session there is no audio path to the user, so a call left up would leave the
// visitor talking to nobody. Every other cause is a media-layer misfortune — a
// foreign OpenWebNet stop frame, an RTP stall, a transport failure, a failed
// startup — and the user may still be mid-conversation, so the call must
// survive it and the card can re-offer to get the picture back.
//
// endsCall is false in the zero value on purpose: a cause added later cannot
// end a call unless it says so. See
// docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md.
type stopCause struct {
	reason   string
	endsCall bool
}

var (
	causeConsumerReleased = stopCause{reason: "consumer released", endsCall: true}
	causeConsumerLeft     = stopCause{reason: "consumer left"}
	causeRemoteBYE        = stopCause{reason: "remote sip bye"}
	causeControlStop      = stopCause{reason: "openwebnet stream stop"}
	causeTracksStalled    = stopCause{reason: "rtp tracks stalled"}
	causeSourceFailure    = stopCause{reason: "managed source failure"}
	causeSourceCreation   = stopCause{reason: "source creation failed"}
	causeStartupFailed    = stopCause{reason: "source startup failed"}
	causeStartupTimedOut  = stopCause{reason: "source startup timed out"}
	causeRingingEnded     = stopCause{reason: "ringing call ended"}
)

// ManagedSourceCallEnder is an optional capability of a managed source that
// carries a SIP call it did not create. Such a source keeps the call up through
// its own teardown unless finishStop tells it otherwise, which it does only for
// a teardown a consumer asked for.
type ManagedSourceCallEnder interface {
	EndCallOnClose()
}

// ManagedSourceRingingPreview is the capability a source needs to serve a lease
// taken while the inbound call still rings. PrepareRingingPreview runs before
// Start and makes the source borrow the call and start video only; Answered
// adds audio once the call is answered. See
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
type ManagedSourceRingingPreview interface {
	PrepareRingingPreview()
	Answered(context.Context) error
}

type StreamOwner string

type StreamHealth string

const (
	StreamOwnerIdle      StreamOwner = "idle"
	StreamOwnerCompanion StreamOwner = "companion"
	StreamOwnerExternal  StreamOwner = "external"
)

const (
	StreamHealthStarting  StreamHealth = "starting"
	StreamHealthLive      StreamHealth = "live"
	StreamHealthVideoOnly StreamHealth = "video_only"
	StreamHealthAudioOnly StreamHealth = "audio_only"
	StreamHealthStalled   StreamHealth = "stalled"
)

type TrackState struct {
	Requested    bool
	RequestedAt  time.Time
	FirstPacket  time.Time
	LastPacket   time.Time
	SSRC         uint32
	Sequence     uint16
	PacketCount  uint64
	InvalidCount uint64
}

func (t TrackState) Flowing(now time.Time, timeout time.Duration) bool {
	return !t.LastPacket.IsZero() && now.Sub(t.LastPacket) <= timeout
}

type StreamSnapshot struct {
	LeaseID      uint64
	Owner        StreamOwner
	EntrypointID string
	DevAddr      string
	Video        TrackState
	Audio        TrackState
	Health       StreamHealth
}

// StreamLease is one consumer's hold on the stream: id names the stream and
// consumer the holder. Stream-level paths build it with consumer 0, which never
// names a consumer.
type StreamLease struct{ id, consumer uint64 }

// acquireMode says what an acquire may do. A shareable acquire joins a running
// stream born from another shareable acquire instead of failing busy; see
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
type acquireMode struct {
	allowRinging bool
	shareable    bool
}

// streamStart tells consumers joining a stream that its startup is over, and
// how it went.
type streamStart struct {
	done chan struct{}
	once sync.Once
	err  error
}

func newStreamStart() *streamStart {
	return &streamStart{done: make(chan struct{})}
}

func (s *streamStart) finish(err error) {
	s.once.Do(func() {
		s.err = err
		close(s.done)
	})
}

// reservation is what reserve hands acquire: a new stream to start, or a
// running one to join.
type reservation struct {
	lease   *StreamLease
	preview bool
	join    bool
	fanout  *fanout
	start   *streamStart
}

type SourceEvents struct {
	VideoRTP  func(*rtp.Packet)
	AudioRTP  func(*rtp.Packet)
	RemoteBYE func()
	Failed    func(error)
}

// ManagedSource is the transport layer for a single already-authorized source.
type ManagedSource interface {
	Start(context.Context) error
	Close(context.Context) error
}

// ManagedSourceBackchannel is an optional uplink capability of a managed source.
type ManagedSourceBackchannel interface {
	WriteBackchannelRTP(*rtp.Packet) error
}

type (
	ManagedSourceFactory func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error)
	StreamStateObserver  func(StreamSnapshot)
)

// StreamCoordinator is the sole owner of the intercom's single media source.
// OpenWebNet frames describe requested tracks; RTP metadata proves actual flow.
type StreamCoordinator struct {
	mu      sync.Mutex
	nextID  uint64
	leaseID uint64
	// controlLeaseID identifies the lease that emitted the currently observed
	// OpenWebNet stream-start frames. Stops without a matching start are stale.
	controlLeaseID uint64
	snapshot       StreamSnapshot
	factory        ManagedSourceFactory
	source         ManagedSource
	cleanup        func()
	starting       bool
	stopping       bool
	observer       StreamStateObserver
	logger         *slog.Logger
	// answeredCallProbe reports whether a stream seen as external belongs to an
	// inbound call the companion has answered. Nil until the application wires
	// signaling in, and a nil probe keeps the original behaviour of refusing
	// every externally owned stream.
	answeredCallProbe func() bool

	// ringingCallProbe reports whether an inbound call is ringing and not yet
	// answered. While it is, only a ringing preview may take a lease.
	ringingCallProbe func() bool

	// previewRinging marks the current lease as a ringing preview not yet
	// promoted: backchannel blocked, StopRinging applies. promotePending records
	// a CallAnswered that arrived while that lease was still starting.
	previewRinging bool
	promotePending bool

	// leaseEvents are the current stream's events as wrapped by sourceEvents.
	// Promotion failure goes through them.
	leaseEvents SourceEvents

	// shared marks the current stream as one a shareable acquire may join;
	// fanout delivers its media to every consumer; start tells joiners when its
	// startup is over. See
	// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
	shared       bool
	fanout       *fanout
	start        *streamStart
	nextConsumer uint64

	// talker is the consumer that owns the backchannel (0 = none): the intercom
	// has one Opus decoder, so a second microphone would be mixed into the first.
	// The first consumer to send audio owns it until it leaves.
	talker uint64

	// promoteCancel cancels the promotion in flight, if any. stop takes it so a
	// lease torn down mid-promotion does not keep retrying the audio request
	// against a closed session.
	promoteCancel context.CancelFunc
}

// SetStateObserver receives source transitions after their coordinator state is committed.
func (c *StreamCoordinator) SetStateObserver(observer StreamStateObserver) {
	c.mu.Lock()
	c.observer = observer
	snapshot := c.snapshot
	c.mu.Unlock()

	if observer != nil {
		observer(snapshot)
	}
}

// SetAnsweredCallProbe supplies the predicate that tells the coordinator whether
// a stream it sees as external belongs to a call the companion has answered.
// Such a stream is the companion's to take: the intercom starts its AV while
// ringing, long before the user answers, so without this an answered call could
// never reach a lease.
func (c *StreamCoordinator) SetAnsweredCallProbe(probe func() bool) {
	c.mu.Lock()
	c.answeredCallProbe = probe
	c.mu.Unlock()
}

// answeredCall runs the probe with c.mu released.
//
// The probe locks the signaling manager. Calling it under c.mu would establish
// a c.mu -> m.mu order that no current path takes in reverse; keeping the call
// outside the lock is what stops that edge from ever mattering. The verdict is
// therefore taken a moment before the critical section that uses it; a call
// cannot end in that window without the ordinary teardown path observing it.
func (c *StreamCoordinator) answeredCall() bool {
	c.mu.Lock()
	probe := c.answeredCallProbe
	c.mu.Unlock()

	return probe != nil && probe()
}

// SetRingingCallProbe supplies the predicate that tells the coordinator whether
// an inbound call is ringing. While it is, only AcquireRingingPreview may take a
// lease: RTSP and HomeKit wait for the answer.
func (c *StreamCoordinator) SetRingingCallProbe(probe func() bool) {
	c.mu.Lock()
	c.ringingCallProbe = probe
	c.mu.Unlock()
}

// ringingCall runs the probe with c.mu released, for the reason given on
// answeredCall.
func (c *StreamCoordinator) ringingCall() bool {
	c.mu.Lock()
	probe := c.ringingCallProbe
	c.mu.Unlock()

	return probe != nil && probe()
}

func NewStreamCoordinator(logger *slog.Logger, factory ManagedSourceFactory) *StreamCoordinator {
	if logger == nil {
		logger = slog.Default()
	}

	return &StreamCoordinator{factory: factory, logger: logger.With("component", "media.coordinator"), snapshot: StreamSnapshot{Owner: StreamOwnerIdle}}
}

func (c *StreamCoordinator) Acquire(ctx context.Context, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error) {
	return c.acquire(ctx, ctx, entrypoint, events, acquireMode{})
}

// AcquireWithStartup bounds source initialization without tying the live lease
// to the caller's short-lived startup deadline.
func (c *StreamCoordinator) AcquireWithStartup(lifecycleCtx, startupCtx context.Context, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error) {
	return c.acquire(lifecycleCtx, startupCtx, entrypoint, events, acquireMode{})
}

// AcquireRingingPreview is AcquireWithStartup for the consumers that may also
// take the stream while the inbound call rings: a WebRTC viewer, which can then
// answer on the same session, and the ring holder. The lease starts video only
// and is promoted by PromoteRinging or stopped by StopRinging. Outside a ring it
// is an ordinary lease.
//
// It is also the shareable acquire: it joins a stream another shareable
// consumer of the same entrypoint already holds, so two devices watch together
// instead of the second closing the first. See
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
func (c *StreamCoordinator) AcquireRingingPreview(lifecycleCtx, startupCtx context.Context, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error) {
	return c.acquire(lifecycleCtx, startupCtx, entrypoint, events, acquireMode{allowRinging: true, shareable: true})
}

func (c *StreamCoordinator) acquire(lifecycleCtx, startupCtx context.Context, entrypoint config.Entrypoint, events SourceEvents, mode acquireMode) (*StreamLease, error) {
	r, err := c.reserve(lifecycleCtx, entrypoint, mode)
	if err != nil {
		return nil, err
	}

	if r.join {
		return c.join(lifecycleCtx, startupCtx, r, entrypoint, events)
	}

	lease := r.lease

	// The cache is empty and nothing flows before Start, so this replays
	// nothing. It fails only if the ring ended while the stream was reserved.
	if !r.fanout.add(lease.consumer, events) {
		return nil, ErrStreamStopped
	}

	wrapped := c.sourceEvents(lease, r.fanout)

	source, cleanup, err := c.factory(entrypoint, wrapped)
	if err != nil || source == nil {
		c.finishStop(lease, nil, cleanup, causeSourceCreation)

		if err != nil {
			c.logger.ErrorContext(lifecycleCtx, "media source creation failed", "lease_id", lease.id, "entrypoint_id", entrypoint.ID, "error", err)
			return nil, err
		}

		c.logger.ErrorContext(lifecycleCtx, "media source creation failed", "lease_id", lease.id, "entrypoint_id", entrypoint.ID, "error", "factory returned nil")

		return nil, ErrStreamBusy
	}

	if r.preview {
		ringingSource, ok := source.(ManagedSourceRingingPreview)
		if !ok {
			c.finishStop(lease, source, cleanup, causeSourceCreation)
			c.logger.ErrorContext(lifecycleCtx, "media source creation failed", "lease_id", lease.id, "entrypoint_id", entrypoint.ID, "error", "source cannot serve a ringing preview")

			return nil, ErrStreamBusy
		}

		ringingSource.PrepareRingingPreview()
	}

	if !c.attachSource(lease, source, cleanup, wrapped) {
		return nil, ErrStreamBusy
	}

	if err := c.startSource(lifecycleCtx, startupCtx, lease, source, entrypoint.ID); err != nil {
		return nil, err
	}

	c.finishStart(lease)
	go c.watch(lifecycleCtx, lease)

	return lease, nil
}

// join waits for the stream's startup, within the joiner's own startup budget,
// and adds the consumer to it. A failed startup, or a stream that stops before
// the consumer is in, fails the join.
func (c *StreamCoordinator) join(ctx, startupCtx context.Context, r reservation, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error) {
	select {
	case <-r.start.done:
	case <-startupCtx.Done():
		return nil, startupCtx.Err()
	}

	if r.start.err != nil {
		return nil, r.start.err
	}

	if !r.fanout.add(r.lease.consumer, events) {
		return nil, ErrStreamStopped
	}

	c.logger.InfoContext(ctx, "stream consumer joined", "lease_id", r.lease.id, "consumer_id", r.lease.consumer, "entrypoint_id", entrypoint.ID)

	return r.lease, nil
}

// reserve decides who may take the stream. While the inbound call rings only a
// ringing preview may, whatever the stream owner is: a card that closes and
// reopens its preview mid-ring finds the owner idle, and RTSP must still wait.
// A shareable acquire finding a shareable stream of the same entrypoint joins it.
func (c *StreamCoordinator) reserve(ctx context.Context, entrypoint config.Entrypoint, mode acquireMode) (reservation, error) {
	ringing := c.ringingCall()
	answered := c.answeredCall()

	c.mu.Lock()
	if ringing && !mode.allowRinging {
		c.mu.Unlock()
		c.logger.DebugContext(ctx, "stream lease rejected", "entrypoint_id", entrypoint.ID, "reason", "inbound_call_ringing")

		return reservation{}, ErrRingingCall
	}

	adopted := c.snapshot.Owner == StreamOwnerExternal

	if adopted && !answered && !ringing {
		c.mu.Unlock()
		c.logger.DebugContext(ctx, "stream lease rejected", "entrypoint_id", entrypoint.ID, "reason", "external_stream_active")

		return reservation{}, ErrExternalStream
	}

	if c.leaseID != 0 && mode.shareable && c.shared && !c.stopping && c.snapshot.EntrypointID == entrypoint.ID {
		c.nextConsumer++
		r := reservation{lease: &StreamLease{id: c.leaseID, consumer: c.nextConsumer}, join: true, fanout: c.fanout, start: c.start}
		c.mu.Unlock()
		c.logger.DebugContext(ctx, "stream consumer joining", "lease_id", r.lease.id, "consumer_id", r.lease.consumer, "entrypoint_id", entrypoint.ID)

		return r, nil
	}

	if c.leaseID != 0 || c.factory == nil {
		c.mu.Unlock()
		c.logger.DebugContext(ctx, "stream lease rejected", "entrypoint_id", entrypoint.ID, "reason", "source_unavailable", "owner", c.snapshot.Owner)

		return reservation{}, ErrStreamBusy
	}

	c.nextID++
	c.nextConsumer++
	c.leaseID = c.nextID
	c.controlLeaseID = 0
	c.starting = true
	c.previewRinging = ringing
	c.promotePending = false
	c.shared = mode.shareable
	c.talker = 0
	c.fanout = newFanout()
	c.start = newStreamStart()
	c.snapshot = StreamSnapshot{LeaseID: c.leaseID, Owner: StreamOwnerCompanion, EntrypointID: entrypoint.ID, DevAddr: entrypoint.DevAddr, Health: StreamHealthStarting}
	r := reservation{lease: &StreamLease{id: c.leaseID, consumer: c.nextConsumer}, preview: ringing, fanout: c.fanout, start: c.start}
	c.mu.Unlock()
	c.logger.InfoContext(ctx, "stream lease acquired", "lease_id", r.lease.id, "entrypoint_id", entrypoint.ID, "adopted_external", adopted, "ringing_preview", ringing, "shared", mode.shareable)

	return r, nil
}

// sourceEvents wraps the stream's fan-out: every consumer gets the media, and a
// remote BYE or a source failure stops the stream and is told to all of them.
func (c *StreamCoordinator) sourceEvents(lease *StreamLease, f *fanout) SourceEvents {
	return SourceEvents{
		VideoRTP: func(packet *rtp.Packet) {
			c.ObserveRTPPacket(lease, true, packet)
			f.video(packet)
		},
		AudioRTP: func(packet *rtp.Packet) {
			c.ObserveRTPPacket(lease, false, packet)
			f.audio(packet)
		},
		RemoteBYE: func() {
			c.stop(lease, causeRemoteBYE)
			f.remoteBYE()
		},
		Failed: func(err error) {
			c.logger.Error("media source failed", "lease_id", lease.id, "error", err)
			c.stop(lease, causeSourceFailure)
			f.failed(err)
		},
	}
}

func (c *StreamCoordinator) attachSource(lease *StreamLease, source ManagedSource, cleanup func(), wrapped SourceEvents) bool {
	c.mu.Lock()
	if c.leaseID != lease.id {
		c.mu.Unlock()

		_ = source.Close(context.Background())

		if cleanup != nil {
			cleanup()
		}

		return false
	}

	c.source, c.cleanup = source, cleanup
	c.leaseEvents = wrapped
	c.mu.Unlock()

	return true
}

func (c *StreamCoordinator) startSource(lifecycleCtx, startupCtx context.Context, lease *StreamLease, source ManagedSource, entrypointID string) error {
	startResult := make(chan error, 1)
	go func() {
		startResult <- source.Start(lifecycleCtx)
	}()

	select {
	case err := <-startResult:
		if err != nil {
			c.logger.ErrorContext(lifecycleCtx, "media source start failed", "lease_id", lease.id, "entrypoint_id", entrypointID, "error", err)
			c.stop(lease, causeStartupFailed)

			return err
		}
	case <-startupCtx.Done():
		// Teardown may need to wait for a cooperative source startup, but the
		// signaling request must not remain blocked beyond its startup budget.
		go c.stop(lease, causeStartupTimedOut)
		return startupCtx.Err()
	}

	return nil
}

func (c *StreamCoordinator) finishStart(lease *StreamLease) {
	// Sampled before c.mu, for the reason given on answeredCall. A call answered
	// between reserve's ringing probe and now, while the drain's PromoteRinging
	// found no lease, would otherwise leave this lease a preview for the whole
	// call. A later CallAnswered event finds previewRinging already false.
	answered := c.answeredCall()

	c.mu.Lock()

	var (
		snapshot StreamSnapshot
		observer StreamStateObserver
		promote  bool
		pSource  ManagedSourceRingingPreview
		pFailed  func(error)
		pCtx     context.Context
		start    *streamStart
	)

	if c.leaseID == lease.id {
		c.starting = false
		start = c.start
		snapshot = c.snapshot
		observer = c.observer

		// The call was answered while this preview was starting, or before it
		// had a lease at all.
		if c.previewRinging && (c.promotePending || answered) && !c.stopping {
			pSource, pFailed, pCtx = c.takePromotionLocked()
			promote = true
		}
	}
	c.mu.Unlock()

	if start != nil {
		start.finish(nil)
	}

	if observer != nil {
		observer(snapshot)
	}

	if promote {
		go c.promote(lease, pCtx, pSource, pFailed)
	}
}

func (c *StreamCoordinator) watch(ctx context.Context, lease *StreamLease) {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if c.Reconcile(lease, now) {
				return
			}
		}
	}
}

// Reconcile derives stream health and stops a source only after both requested
// tracks have remained inactive for the configured confirmation window.
func (c *StreamCoordinator) Reconcile(lease *StreamLease, now time.Time) bool {
	if lease == nil {
		return true
	}

	c.mu.Lock()
	if c.leaseID != lease.id {
		c.mu.Unlock()
		return true
	}

	c.snapshot.Health = streamHealth(c.snapshot, now)
	stalled := c.snapshot.Health == StreamHealthStalled
	videoLastPacket := c.snapshot.Video.LastPacket
	audioLastPacket := c.snapshot.Audio.LastPacket
	c.mu.Unlock()

	if stalled {
		c.logger.Warn("stream stalled; stopping source", "lease_id", lease.id, "video_last_packet_at", videoLastPacket, "audio_last_packet_at", audioLastPacket)
		c.stop(lease, causeTracksStalled)

		return true
	}

	return false
}

// Release lets one consumer go. The last one out ends the stream and, with it,
// the SIP call behind it: that is the teardown a consumer that lets go of the
// stream asks for. A consumer that steps aside without ending the call uses
// Leave instead.
func (c *StreamCoordinator) Release(lease *StreamLease) bool {
	return c.leave(lease, causeConsumerReleased)
}

// Leave lets one consumer go without ever ending the call: the last one out
// stops the stream with no BYE. The ring holder leaves this way, and so does a
// card that closes because the call was answered on another device. See
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
func (c *StreamCoordinator) Leave(lease *StreamLease) bool {
	return c.leave(lease, causeConsumerLeft)
}

func (c *StreamCoordinator) leave(lease *StreamLease, cause stopCause) bool {
	if lease == nil {
		return false
	}

	c.mu.Lock()
	f := c.fanout

	if c.leaseID != lease.id || f == nil {
		c.mu.Unlock()
		c.logger.Debug("stream consumer release ignored; lease is no longer active", "lease_id", lease.id, "consumer_id", lease.consumer, "reason", cause.reason)

		return false
	}
	c.mu.Unlock()

	remaining, found := f.remove(lease.consumer)
	if !found {
		return false
	}

	// The uplink frees up for the next consumer that speaks. The last-Release
	// rule below is untouched: only remaining decides whether the stream ends.
	c.mu.Lock()
	if c.leaseID == lease.id && c.talker == lease.consumer {
		c.talker = 0
	}
	c.mu.Unlock()

	if remaining > 0 {
		c.logger.Info("stream consumer left", "lease_id", lease.id, "consumer_id", lease.consumer, "reason", cause.reason, "remaining", remaining)

		return true
	}

	return c.stop(lease, cause)
}

// PromoteRinging turns the ringing preview into the answered call: the
// backchannel opens and the source asks the intercom for audio, while video,
// sockets and the consumer's session carry on. It returns at once — the audio
// request can take seconds and the caller is the SIP event drain. A lease still
// starting is promoted as soon as its source is up. A no-op on any other lease.
func (c *StreamCoordinator) PromoteRinging() {
	c.mu.Lock()
	if c.leaseID == 0 || !c.previewRinging || c.stopping {
		c.mu.Unlock()

		return
	}

	if c.starting {
		c.promotePending = true
		c.mu.Unlock()

		return
	}

	lease := &StreamLease{id: c.leaseID}
	source, failed, ctx := c.takePromotionLocked()
	c.mu.Unlock()

	go c.promote(lease, ctx, source, failed)
}

// takePromotionLocked ends the preview phase and returns what promote needs,
// including the promotion's context, whose cancel func stop can reach. c.mu
// must be held.
func (c *StreamCoordinator) takePromotionLocked() (ManagedSourceRingingPreview, func(error), context.Context) {
	c.previewRinging = false
	c.promotePending = false

	// The audio request is sent after startup, when OpenWebNet frames are no
	// longer attributed to the lease, so ObserveControlTrack never records it.
	// Without this the stall watchdog needs a requested audio track it can never
	// see and could not stop a promoted preview.
	c.snapshot.Audio.Requested = true
	if c.snapshot.Audio.RequestedAt.IsZero() {
		c.snapshot.Audio.RequestedAt = time.Now()
	}

	ctx, cancel := context.WithTimeout(context.Background(), promotionTimeout)
	c.promoteCancel = cancel

	source, _ := c.source.(ManagedSourceRingingPreview)

	return source, c.leaseEvents.Failed, ctx
}

// promote asks the source for audio. A failure stops the lease as a source
// failure, which does not end the call: the consumer sees its session close and
// can open a normal one, which adopts the answered call.
func (c *StreamCoordinator) promote(lease *StreamLease, ctx context.Context, source ManagedSourceRingingPreview, failed func(error)) {
	// Releases the timer whatever the outcome; stop's cancel is idempotent with it.
	defer c.releasePromotion(lease)

	if source == nil {
		return
	}

	if err := source.Answered(ctx); err != nil {
		// A lease torn down meanwhile cancelled the request: the failure is the
		// teardown's doing, and its consumer already has its own end signal.
		if errors.Is(ctx.Err(), context.Canceled) || !c.leaseCurrent(lease) {
			c.logger.Debug("ringing preview promotion abandoned; the lease is gone", "lease_id", lease.id, "error", err)

			return
		}

		c.logger.Warn("ringing preview could not join the answered call; the call stays up", "lease_id", lease.id, "error", err)

		if failed != nil {
			failed(fmt.Errorf("promote ringing preview: %w", err))
		}

		return
	}

	c.logger.Info("ringing preview joined the answered call", "lease_id", lease.id)
}

// leaseCurrent reports whether lease is still the live, not-stopping lease.
func (c *StreamCoordinator) leaseCurrent(lease *StreamLease) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.leaseID == lease.id && !c.stopping
}

// releasePromotion cancels the promotion context of lease, freeing its timer.
func (c *StreamCoordinator) releasePromotion(lease *StreamLease) {
	c.mu.Lock()

	var cancel context.CancelFunc
	if c.leaseID == lease.id {
		cancel, c.promoteCancel = c.promoteCancel, nil
	}
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// StopRinging ends a ringing preview because the ring is over without this
// companion answering: cancelled, timed out, answered elsewhere or rejected. It
// never ends the call, and tells every consumer so each can close its session.
// A no-op on any other lease.
func (c *StreamCoordinator) StopRinging() {
	c.mu.Lock()
	if c.leaseID == 0 || !c.previewRinging {
		c.mu.Unlock()

		return
	}

	lease := &StreamLease{id: c.leaseID}
	f := c.fanout
	c.mu.Unlock()

	if !c.stop(lease, causeRingingEnded) {
		return
	}

	if f != nil {
		f.failed(ErrRingingEnded)
	}
}

// ReplayKeyframe sends the cached keyframe again to one consumer. A WebRTC
// joiner calls it once its peer connection is up, because the replay at join
// time is dropped while SRTP is still being set up. A no-op on a stale lease.
func (c *StreamCoordinator) ReplayKeyframe(lease *StreamLease) {
	if lease == nil {
		return
	}

	c.mu.Lock()

	var f *fanout
	if c.leaseID == lease.id && !c.stopping {
		f = c.fanout
	}
	c.mu.Unlock()

	// Outside c.mu: the replay runs consumer callbacks under deliveryMu.
	if f != nil {
		f.replay(lease.consumer)
	}
}

// WriteBackchannelRTP forwards uplink RTP only to the current live source lease,
// and only from the consumer that owns the uplink.
func (c *StreamCoordinator) WriteBackchannelRTP(lease *StreamLease, packet *rtp.Packet) error {
	if lease == nil {
		return ErrBackchannelUnavailable
	}

	c.mu.Lock()
	if c.leaseID != lease.id || c.starting || c.stopping || c.previewRinging {
		c.mu.Unlock()
		return ErrBackchannelUnavailable
	}

	backchannel, ok := c.source.(ManagedSourceBackchannel)
	if !ok || backchannel == nil {
		c.mu.Unlock()
		return ErrBackchannelUnavailable
	}

	if c.talker == 0 {
		c.talker = lease.consumer
	}

	if c.talker != lease.consumer {
		c.mu.Unlock()
		return ErrBackchannelUnavailable
	}
	c.mu.Unlock()

	return backchannel.WriteBackchannelRTP(packet)
}

func (c *StreamCoordinator) stop(lease *StreamLease, cause stopCause) bool {
	if lease == nil {
		return false
	}

	c.mu.Lock()
	if c.leaseID != lease.id {
		c.mu.Unlock()
		c.logger.Debug("stream stop ignored; lease is no longer active", "lease_id", lease.id, "reason", cause.reason)

		return false
	}

	if c.stopping {
		c.mu.Unlock()
		c.logger.Debug("stream stop ignored; teardown already in progress", "lease_id", lease.id, "reason", cause.reason)

		return false
	}

	c.stopping = true
	source, cleanup := c.source, c.cleanup
	c.source, c.cleanup = nil, nil
	promoteCancel := c.promoteCancel
	c.promoteCancel = nil

	// A ringing preview lost to the media layer would otherwise leave its
	// consumers holding dead sessions that the user could still answer on. Told
	// after the teardown so they close their sessions; a card then falls back
	// to answering with a fresh offer, which adopts the answered call.
	// StopRinging notifies for itself, and release or leave never do.
	var lost *fanout
	if c.previewRinging && (cause == causeControlStop || cause == causeTracksStalled) {
		lost = c.fanout
	}

	// No consumer may join a stream that is going away.
	if c.fanout != nil {
		c.fanout.close()
	}
	c.mu.Unlock()

	// Cancelled after unlocking out of habit: cancelling a context runs no
	// coordinator code, so doing it under c.mu would also be safe.
	if promoteCancel != nil {
		promoteCancel()
	}

	c.logger.Info("stream teardown started", "lease_id", lease.id, "reason", cause.reason)
	c.finishStop(lease, source, cleanup, cause)

	if lost != nil {
		lost.failed(fmt.Errorf("%w: %s", ErrRingingPreviewLost, cause.reason))
	}

	return true
}

func (c *StreamCoordinator) finishStop(lease *StreamLease, source ManagedSource, cleanup func(), cause stopCause) {
	var closeErr error

	if source != nil {
		// Told before Close, because Close is where the source decides whether to
		// send a BYE. A source that did not create its dialog keeps the call up
		// unless this ran.
		if cause.endsCall {
			if ender, ok := source.(ManagedSourceCallEnder); ok {
				ender.EndCallOnClose()
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		closeErr = source.Close(ctx)

		cancel()
	}

	if cleanup != nil {
		cleanup()
	}

	c.mu.Lock()

	var start *streamStart

	if c.leaseID == lease.id {
		if c.fanout != nil {
			c.fanout.close()
		}

		start = c.start
		c.leaseID = 0
		c.controlLeaseID = 0
		c.starting = false
		c.stopping = false
		c.previewRinging = false
		c.promotePending = false
		c.promoteCancel = nil
		c.leaseEvents = SourceEvents{}
		c.shared = false
		c.talker = 0
		c.fanout = nil
		c.start = nil
		c.snapshot = StreamSnapshot{Owner: StreamOwnerIdle}
	}

	snapshot := c.snapshot
	observer := c.observer
	c.mu.Unlock()

	// A consumer still waiting to join learns the startup is over for good.
	if start != nil {
		start.finish(ErrStreamStopped)
	}

	if observer != nil {
		observer(snapshot)
	}

	if closeErr != nil {
		c.logger.Error("stream teardown failed", "lease_id", lease.id, "reason", cause.reason, "error", fmt.Errorf("close managed source: %w", closeErr))
		return
	}

	c.logger.Info("stream teardown completed", "lease_id", lease.id, "reason", cause.reason)
}

// ObserveControlTrack marks a requested track from an OpenWebNet AV start frame.
func (c *StreamCoordinator) ObserveControlTrack(video bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.leaseID != 0 {
		if !c.starting {
			c.logger.Debug("openwebnet stream start ignored; source attempt is not starting", "lease_id", c.leaseID)
			return
		}

		c.controlLeaseID = c.leaseID
	}

	c.observeRequestedTrackLocked(video)
}

// ObserveControlStop records OpenWebNet stream teardown. A Companion lease is
// retained until its source has completed local cleanup. The stop must follow
// a start observed for the active source attempt; multicast can otherwise
// replay a stop from an earlier attempt.
func (c *StreamCoordinator) ObserveControlStop() {
	c.mu.Lock()
	if c.leaseID == 0 {
		if c.snapshot.Owner == StreamOwnerExternal {
			c.snapshot = StreamSnapshot{Owner: StreamOwnerIdle}
		}
		c.mu.Unlock()

		return
	}

	if c.controlLeaseID != c.leaseID {
		leaseID := c.leaseID
		c.mu.Unlock()
		c.logger.Debug("openwebnet stream stop ignored; no matching start", "lease_id", leaseID)

		return
	}

	if c.starting {
		leaseID := c.leaseID
		c.mu.Unlock()
		c.logger.Debug("openwebnet stream stop ignored during source startup", "lease_id", leaseID)

		return
	}

	lease := &StreamLease{id: c.leaseID}
	c.mu.Unlock()
	c.stop(lease, causeControlStop)
}

func (c *StreamCoordinator) observeRequestedTrackLocked(video bool) {
	if c.leaseID == 0 && c.snapshot.Owner == StreamOwnerIdle {
		c.snapshot.Owner = StreamOwnerExternal
		c.logger.Warn("external stream detected")
	}

	if video {
		c.snapshot.Video.Requested = true
		if c.snapshot.Video.RequestedAt.IsZero() {
			c.snapshot.Video.RequestedAt = time.Now()
		}

		c.snapshot.Health = streamHealth(c.snapshot, time.Now())
		c.logger.Debug("stream control track requested", "track", "video", "owner", c.snapshot.Owner, "health", c.snapshot.Health)

		return
	}

	c.snapshot.Audio.Requested = true
	if c.snapshot.Audio.RequestedAt.IsZero() {
		c.snapshot.Audio.RequestedAt = time.Now()
	}

	c.snapshot.Health = streamHealth(c.snapshot, time.Now())
	c.logger.Debug("stream control track requested", "track", "audio", "owner", c.snapshot.Owner, "health", c.snapshot.Health)
}

func (c *StreamCoordinator) ObserveRTP(lease *StreamLease, video bool, metadata RTPMetadata) {
	if lease == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.leaseID != lease.id {
		return
	}

	track := &c.snapshot.Audio
	if video {
		track = &c.snapshot.Video
	}

	track.PacketCount = metadata.PacketCount
	track.InvalidCount = metadata.InvalidCount
	track.SSRC = metadata.SSRC

	track.LastPacket = metadata.LastPacketAt
	if track.FirstPacket.IsZero() && !metadata.LastPacketAt.IsZero() {
		track.FirstPacket = metadata.LastPacketAt
	}

	c.snapshot.Health = streamHealth(c.snapshot, time.Now())
}

// ObserveRTPPacket records the valid packets forwarded to media consumers.
// Sequence values are retained for diagnostics; flow is based on arrival time.
func (c *StreamCoordinator) ObserveRTPPacket(lease *StreamLease, video bool, packet *rtp.Packet) {
	if lease == nil || packet == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.leaseID != lease.id {
		return
	}

	track := &c.snapshot.Audio
	if video {
		track = &c.snapshot.Video
	}

	now := time.Now()
	track.PacketCount++
	track.SSRC = packet.SSRC
	track.Sequence = packet.SequenceNumber

	track.LastPacket = now
	if track.FirstPacket.IsZero() {
		track.FirstPacket = now
	}

	c.snapshot.Health = streamHealth(c.snapshot, now)
}

func (c *StreamCoordinator) Snapshot() StreamSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.snapshot
}

func streamHealth(snapshot StreamSnapshot, now time.Time) StreamHealth {
	videoRequested, audioRequested := snapshot.Video.Requested, snapshot.Audio.Requested
	videoFlowing := snapshot.Video.Flowing(now, trackFlowTimeout)

	audioFlowing := snapshot.Audio.Flowing(now, trackFlowTimeout)
	switch {
	case videoFlowing && audioFlowing:
		return StreamHealthLive
	case videoFlowing:
		return StreamHealthVideoOnly
	case audioFlowing:
		return StreamHealthAudioOnly
	case videoRequested && audioRequested:
		if now.Sub(snapshot.Video.RequestedAt) > trackFlowTimeout && now.Sub(snapshot.Audio.RequestedAt) > trackFlowTimeout {
			return StreamHealthStalled
		}

		return StreamHealthStarting
	default:
		return StreamHealthStarting
	}
}

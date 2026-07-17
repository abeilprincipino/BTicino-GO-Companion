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
)

const (
	trackFlowTimeout = 5 * time.Second
	watchInterval    = time.Second
)

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
	Owner        StreamOwner
	EntrypointID string
	DevAddr      string
	Video        TrackState
	Audio        TrackState
	Health       StreamHealth
}

type StreamLease struct{ id uint64 }

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

type ManagedSourceFactory func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error)

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
	logger         *slog.Logger
}

func NewStreamCoordinator(logger *slog.Logger, factory ManagedSourceFactory) *StreamCoordinator {
	if logger == nil {
		logger = slog.Default()
	}
	return &StreamCoordinator{factory: factory, logger: logger.With("component", "media.coordinator"), snapshot: StreamSnapshot{Owner: StreamOwnerIdle}}
}

func (c *StreamCoordinator) Acquire(ctx context.Context, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error) {
	c.mu.Lock()
	if c.snapshot.Owner == StreamOwnerExternal {
		c.mu.Unlock()
		c.logger.WarnContext(ctx, "stream lease rejected; external stream active", "entrypoint_id", entrypoint.ID, "dev_addr", entrypoint.DevAddr)
		return nil, ErrExternalStream
	}
	if c.leaseID != 0 || c.factory == nil {
		c.mu.Unlock()
		c.logger.WarnContext(ctx, "stream lease rejected; source unavailable", "entrypoint_id", entrypoint.ID, "dev_addr", entrypoint.DevAddr, "owner", c.snapshot.Owner)
		return nil, ErrStreamBusy
	}
	c.nextID++
	c.leaseID = c.nextID
	c.controlLeaseID = 0
	c.starting = true
	c.snapshot = StreamSnapshot{Owner: StreamOwnerCompanion, EntrypointID: entrypoint.ID, DevAddr: entrypoint.DevAddr, Health: StreamHealthStarting}
	lease := &StreamLease{id: c.leaseID}
	c.mu.Unlock()
	c.logger.InfoContext(ctx, "stream lease acquired", "lease_id", lease.id, "entrypoint_id", entrypoint.ID, "dev_addr", entrypoint.DevAddr, "health", StreamHealthStarting)

	source, cleanup, err := c.factory(entrypoint, SourceEvents{
		VideoRTP: func(packet *rtp.Packet) {
			c.ObserveRTPPacket(lease, true, packet)
			if events.VideoRTP != nil {
				events.VideoRTP(packet)
			}
		},
		AudioRTP: func(packet *rtp.Packet) {
			c.ObserveRTPPacket(lease, false, packet)
			if events.AudioRTP != nil {
				events.AudioRTP(packet)
			}
		},
		RemoteBYE: func() {
			c.stop(lease, "remote sip bye")
			if events.RemoteBYE != nil {
				events.RemoteBYE()
			}
		},
		Failed: func(err error) {
			c.logger.Error("managed source failed", "lease_id", lease.id, "error", err)
			c.stop(lease, "managed source failure")
			if events.Failed != nil {
				events.Failed(err)
			}
		},
	})
	if err != nil || source == nil {
		c.finishStop(lease, nil, cleanup, "source creation failed")
		if err != nil {
			c.logger.ErrorContext(ctx, "create managed source", "lease_id", lease.id, "error", err)
			return nil, err
		}
		c.logger.ErrorContext(ctx, "create managed source returned nil", "lease_id", lease.id)
		return nil, ErrStreamBusy
	}
	c.mu.Lock()
	if c.leaseID != lease.id {
		c.mu.Unlock()
		_ = source.Close(context.Background())
		if cleanup != nil {
			cleanup()
		}
		return nil, ErrStreamBusy
	}
	c.source, c.cleanup = source, cleanup
	c.mu.Unlock()
	if err := source.Start(ctx); err != nil {
		c.logger.ErrorContext(ctx, "start managed source", "lease_id", lease.id, "error", err)
		c.stop(lease, "source startup failed")
		return nil, err
	}
	c.mu.Lock()
	if c.leaseID == lease.id {
		c.starting = false
	}
	c.mu.Unlock()
	go c.watch(ctx, lease)
	return lease, nil
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
		c.stop(lease, "rtp tracks stalled")
		return true
	}
	return false
}

func (c *StreamCoordinator) Release(lease *StreamLease) bool {
	return c.stop(lease, "consumer released")
}

// WriteBackchannelRTP forwards uplink RTP only to the current live source lease.
func (c *StreamCoordinator) WriteBackchannelRTP(lease *StreamLease, packet *rtp.Packet) error {
	if lease == nil {
		return ErrBackchannelUnavailable
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.leaseID != lease.id || c.starting || c.stopping {
		return ErrBackchannelUnavailable
	}
	backchannel, ok := c.source.(ManagedSourceBackchannel)
	if !ok || backchannel == nil {
		return ErrBackchannelUnavailable
	}
	return backchannel.WriteBackchannelRTP(packet)
}

func (c *StreamCoordinator) stop(lease *StreamLease, reason string) bool {
	if lease == nil {
		return false
	}
	c.mu.Lock()
	if c.leaseID != lease.id {
		c.mu.Unlock()
		c.logger.Debug("stream stop ignored; lease is no longer active", "lease_id", lease.id, "reason", reason)
		return false
	}
	if c.stopping {
		c.mu.Unlock()
		c.logger.Debug("stream stop ignored; teardown already in progress", "lease_id", lease.id, "reason", reason)
		return false
	}
	c.stopping = true
	source, cleanup := c.source, c.cleanup
	c.source, c.cleanup = nil, nil
	c.mu.Unlock()
	c.logger.Info("stream teardown started", "lease_id", lease.id, "reason", reason)
	c.finishStop(lease, source, cleanup, reason)
	return true
}

func (c *StreamCoordinator) finishStop(lease *StreamLease, source ManagedSource, cleanup func(), reason string) {
	var closeErr error
	if source != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		closeErr = source.Close(ctx)
		cancel()
	}
	if cleanup != nil {
		cleanup()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.leaseID == lease.id {
		c.leaseID = 0
		c.controlLeaseID = 0
		c.starting = false
		c.stopping = false
		c.snapshot = StreamSnapshot{Owner: StreamOwnerIdle}
	}
	if closeErr != nil {
		c.logger.Error("stream teardown failed", "lease_id", lease.id, "reason", reason, "error", fmt.Errorf("close managed source: %w", closeErr))
		return
	}
	c.logger.Info("stream teardown complete", "lease_id", lease.id, "reason", reason)
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
	c.stop(lease, "openwebnet stream stop")
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

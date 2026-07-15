package media

import (
	"context"
	"errors"
	"sync"

	"github.com/pion/rtp"
)

var (
	ErrAudioBridgeUnavailable = errors.New("media: audio bridge is unavailable")
	ErrAudioBridgeStarted     = errors.New("media: audio bridge is already started")
)

type AudioMedia interface {
	RegisterSource(Source) error
	UnregisterSource(Source) bool
	RegisterSessionConsumer(Source, SessionID, Consumer) error
	UnregisterSessionConsumer(Source, SessionID) bool
	Distribute(Source, *rtp.Packet) bool
}

type AudioPipeline interface {
	WriteIntercomSpeex(*rtp.Packet) error
	WriteBackchannelOpus(*rtp.Packet) error
	ReadOpusOut() <-chan *rtp.Packet
	ReadSpeexOut() <-chan *rtp.Packet
	Close() error
}

type GStreamerAudio interface {
	StartAudioBridge(context.Context) (AudioPipeline, error)
}

type AudioBridge struct {
	mu sync.Mutex

	media       AudioMedia
	gstreamer   GStreamerAudio
	intercom    Source
	opus        Source
	backchannel Backchannel
	sessionID   SessionID
	pipeline    AudioPipeline
	cancel      context.CancelFunc
	done        chan struct{}
}

func NewAudioBridge(
	media AudioMedia,
	gstreamer GStreamerAudio,
	intercom, opus Source,
	backchannel Backchannel,
	sessionID SessionID,
) *AudioBridge {
	return &AudioBridge{
		media:       media,
		gstreamer:   gstreamer,
		intercom:    intercom,
		opus:        opus,
		backchannel: backchannel,
		sessionID:   sessionID,
	}
}

func (b *AudioBridge) Start(ctx context.Context) error {
	if b == nil || b.media == nil || b.gstreamer == nil || b.sessionID == "" {
		return ErrAudioBridgeUnavailable
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.pipeline != nil {
		return ErrAudioBridgeStarted
	}

	pipeline, err := b.gstreamer.StartAudioBridge(ctx)
	if err != nil {
		return err
	}

	if pipeline == nil {
		return ErrAudioBridgeUnavailable
	}

	if err := b.media.RegisterSource(b.opus); err != nil {
		_ = pipeline.Close()
		return err
	}

	if err := b.media.RegisterSessionConsumer(b.intercom, b.sessionID, ConsumerFunc(func(packet Packet) {
		_ = pipeline.WriteIntercomSpeex(packet.RTP)
	})); err != nil {
		b.media.UnregisterSource(b.opus)

		_ = pipeline.Close()

		return err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	b.pipeline = pipeline
	b.cancel = cancel

	b.done = make(chan struct{})
	go b.forward(runCtx, pipeline, b.done)

	return nil
}

func (b *AudioBridge) WriteRTP(packet *rtp.Packet) error {
	return b.WriteBackchannelOpus(packet)
}

func (b *AudioBridge) WriteBackchannelOpus(packet *rtp.Packet) error {
	b.mu.Lock()
	pipeline := b.pipeline
	b.mu.Unlock()

	if pipeline == nil {
		return ErrAudioBridgeUnavailable
	}

	return pipeline.WriteBackchannelOpus(packet)
}

func (b *AudioBridge) Stop() error {
	if b == nil {
		return nil
	}

	b.mu.Lock()

	pipeline := b.pipeline
	if pipeline == nil {
		b.mu.Unlock()
		return nil
	}

	b.pipeline = nil
	cancel := b.cancel
	done := b.done
	b.cancel = nil
	b.done = nil
	b.mu.Unlock()

	b.media.UnregisterSessionConsumer(b.intercom, b.sessionID)
	b.media.UnregisterSource(b.opus)
	cancel()

	err := pipeline.Close()

	<-done

	return err
}

func (b *AudioBridge) forward(ctx context.Context, pipeline AudioPipeline, done chan struct{}) {
	defer close(done)

	opusOut := pipeline.ReadOpusOut()

	speexOut := pipeline.ReadSpeexOut()
	for opusOut != nil || speexOut != nil {
		select {
		case <-ctx.Done():
			return
		case packet, ok := <-opusOut:
			if !ok {
				opusOut = nil
				continue
			}

			if packet != nil {
				b.media.Distribute(b.opus, packet)
			}
		case packet, ok := <-speexOut:
			if !ok {
				speexOut = nil
				continue
			}

			if packet != nil && b.backchannel != nil {
				_ = b.backchannel.WriteRTP(packet)
			}
		}
	}
}

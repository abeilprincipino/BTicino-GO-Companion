package media

import (
	"bticino-go-companion/internal/core"
	"errors"
	"sync"

	"github.com/pion/rtp"
)

var (
	ErrInvalidSource       = errors.New("media: invalid source")
	ErrInvalidConsumerID   = errors.New("media: invalid consumer id")
	ErrNilConsumer         = errors.New("media: nil consumer")
	ErrSourceNotRegistered = errors.New("media: source is not registered")
)

type MediaKind uint8

const (
	MediaKindUnknown MediaKind = iota
	MediaKindAudio
	MediaKindAudioOpus
	MediaKindVideo
)

type Generation string

type Source struct {
	EntrypointID core.EntrypointID
	MediaKind    MediaKind
	SSRC         uint32
	Generation   Generation
}

type Packet struct {
	Source Source
	RTP    *rtp.Packet
}

type Consumer interface {
	Consume(Packet)
}

type ConsumerFunc func(Packet)

func (f ConsumerFunc) Consume(packet Packet) {
	f(packet)
}

type Distributor struct {
	mu sync.RWMutex

	sources   map[sourceKey]Source
	consumers map[string]Consumer
	sessions  map[sessionKey]Consumer
}

type SessionID string

type sourceKey struct {
	entrypointID core.EntrypointID
	mediaKind    MediaKind
}

type sessionKey struct {
	source  Source
	session SessionID
}

func NewDistributor() *Distributor {
	return &Distributor{
		sources:   map[sourceKey]Source{},
		consumers: map[string]Consumer{},
		sessions:  map[sessionKey]Consumer{},
	}
}

func (d *Distributor) RegisterSource(source Source) error {
	if !source.valid() {
		return ErrInvalidSource
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.sources == nil {
		d.sources = map[sourceKey]Source{}
	}

	d.sources[source.key()] = source

	return nil
}

func (d *Distributor) UnregisterSource(source Source) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	registered, ok := d.sources[source.key()]
	if !ok || registered != source {
		return false
	}

	delete(d.sources, source.key())

	return true
}

func (d *Distributor) ActiveSource(entrypointID core.EntrypointID, mediaKind MediaKind) (Source, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	source, ok := d.sources[sourceKey{entrypointID: entrypointID, mediaKind: mediaKind}]

	return source, ok
}

func (d *Distributor) RegisterConsumer(id string, consumer Consumer) error {
	if id == "" {
		return ErrInvalidConsumerID
	}

	if consumer == nil {
		return ErrNilConsumer
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.consumers == nil {
		d.consumers = map[string]Consumer{}
	}

	d.consumers[id] = consumer

	return nil
}

func (d *Distributor) UnregisterConsumer(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.consumers[id]; !ok {
		return false
	}

	delete(d.consumers, id)

	return true
}

func (d *Distributor) RegisterSessionConsumer(source Source, sessionID SessionID, consumer Consumer) error {
	if sessionID == "" {
		return ErrInvalidConsumerID
	}

	if consumer == nil {
		return ErrNilConsumer
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if registered, ok := d.sources[source.key()]; !ok || registered != source {
		return ErrSourceNotRegistered
	}

	if d.sessions == nil {
		d.sessions = map[sessionKey]Consumer{}
	}

	d.sessions[sessionKey{source: source, session: sessionID}] = consumer

	return nil
}

func (d *Distributor) UnregisterSessionConsumer(source Source, sessionID SessionID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	key := sessionKey{source: source, session: sessionID}
	if _, ok := d.sessions[key]; !ok {
		return false
	}

	delete(d.sessions, key)

	return true
}

func (d *Distributor) Distribute(source Source, packet *rtp.Packet) bool {
	if packet == nil {
		return false
	}

	d.mu.RLock()

	registered, ok := d.sources[source.key()]
	if !ok || registered != source || packet.SSRC != source.SSRC {
		d.mu.RUnlock()
		return false
	}

	consumers := make([]Consumer, 0, len(d.consumers))
	for _, consumer := range d.consumers {
		consumers = append(consumers, consumer)
	}

	for key, consumer := range d.sessions {
		if key.source == source {
			consumers = append(consumers, consumer)
		}
	}

	d.mu.RUnlock()

	for _, consumer := range consumers {
		consumer.Consume(Packet{
			Source: source,
			RTP:    packet.Clone(),
		})
	}

	return true
}

func (s Source) key() sourceKey {
	return sourceKey{
		entrypointID: s.EntrypointID,
		mediaKind:    s.MediaKind,
	}
}

func (s Source) valid() bool {
	return s.EntrypointID != "" &&
		s.MediaKind != MediaKindUnknown &&
		s.SSRC != 0 &&
		s.Generation != ""
}

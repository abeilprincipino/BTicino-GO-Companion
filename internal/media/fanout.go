package media

import (
	"sync"

	"github.com/pion/rtp"
)

// keyframeCacheLimit bounds the video kept for consumers that join a running
// stream. A low-resolution intercom GOP stays far below it; past it the cache
// is dropped and waits for the next keyframe.
const keyframeCacheLimit = 512 * 1024

// fanoutConsumer is one consumer of a shared stream.
type fanoutConsumer struct {
	id     uint64
	events SourceEvents
}

// fanout delivers one stream's media to every consumer that holds it, and
// replays the current H.264 keyframe to a consumer that joins mid-stream so its
// picture appears at once instead of at the intercom's next keyframe. See
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
//
// deliveryMu serialises video delivery with a join's replay: a joiner gets the
// cached packets and then the live ones, with no gap and no overlap in the
// sequence numbers. listMu guards the consumer list alone, and remove takes
// only listMu, so a consumer may leave from inside a media callback.
//
// Callbacks run with deliveryMu held and must not call back into the stream
// coordinator synchronously: the same rule the receiver goroutines already
// impose today.
type fanout struct {
	deliveryMu sync.Mutex
	cache      []*rtp.Packet
	cacheBytes int

	listMu    sync.Mutex
	consumers []fanoutConsumer
	closed    bool
}

func newFanout() *fanout {
	return &fanout{}
}

// add replays the cached keyframe to a consumer and then joins it to the live
// stream. It fails once the fan-out is closed: the stream stopped while the
// consumer was joining.
func (f *fanout) add(id uint64, events SourceEvents) bool {
	f.deliveryMu.Lock()
	defer f.deliveryMu.Unlock()

	if f.isClosed() {
		return false
	}

	if events.VideoRTP != nil {
		for _, packet := range f.cache {
			events.VideoRTP(packet)
		}
	}

	f.listMu.Lock()
	defer f.listMu.Unlock()

	if f.closed {
		return false
	}

	f.consumers = append(f.consumers, fanoutConsumer{id: id, events: events})

	return true
}

// remove detaches a consumer and reports how many are left.
func (f *fanout) remove(id uint64) (remaining int, found bool) {
	f.listMu.Lock()
	defer f.listMu.Unlock()

	for i, consumer := range f.consumers {
		if consumer.id == id {
			// A fresh backing array: a snapshot being iterated keeps its own.
			f.consumers = append(f.consumers[:i:i], f.consumers[i+1:]...)

			return len(f.consumers), true
		}
	}

	return len(f.consumers), false
}

// close stops further joins. The list is kept, so the end of the stream can
// still be told to every consumer on it.
func (f *fanout) close() {
	f.listMu.Lock()
	f.closed = true
	f.listMu.Unlock()
}

func (f *fanout) isClosed() bool {
	f.listMu.Lock()
	defer f.listMu.Unlock()

	return f.closed
}

// snapshot returns the current consumers. The slice is never written in place,
// so it can be iterated without a lock.
func (f *fanout) snapshot() []fanoutConsumer {
	f.listMu.Lock()
	defer f.listMu.Unlock()

	return f.consumers
}

func (f *fanout) video(packet *rtp.Packet) {
	f.deliveryMu.Lock()
	defer f.deliveryMu.Unlock()

	f.remember(packet)

	for _, consumer := range f.snapshot() {
		if consumer.events.VideoRTP != nil {
			consumer.events.VideoRTP(packet)
		}
	}
}

func (f *fanout) audio(packet *rtp.Packet) {
	for _, consumer := range f.snapshot() {
		if consumer.events.AudioRTP != nil {
			consumer.events.AudioRTP(packet)
		}
	}
}

func (f *fanout) remoteBYE() {
	for _, consumer := range f.snapshot() {
		if consumer.events.RemoteBYE != nil {
			consumer.events.RemoteBYE()
		}
	}
}

func (f *fanout) failed(err error) {
	for _, consumer := range f.snapshot() {
		if consumer.events.Failed != nil {
			consumer.events.Failed(err)
		}
	}
}

// remember keeps the video from the latest keyframe onward. SPS, PPS and IDR
// of one access unit share a timestamp, so a keyframe start with the timestamp
// already at the head of the cache continues it rather than restarting it. The
// receiver reuses its read buffer, so the cache holds clones.
func (f *fanout) remember(packet *rtp.Packet) {
	keyframe := startsKeyframe(packet.Payload)
	if keyframe && (len(f.cache) == 0 || f.cache[0].Timestamp != packet.Timestamp) {
		f.cache, f.cacheBytes = f.cache[:0], 0
	}

	if len(f.cache) == 0 && !keyframe {
		return
	}

	if f.cacheBytes+len(packet.Payload) > keyframeCacheLimit {
		f.cache, f.cacheBytes = nil, 0

		return
	}

	f.cache = append(f.cache, packet.Clone())
	f.cacheBytes += len(packet.Payload)
}

// startsKeyframe reports whether an H.264 RTP payload (RFC 6184) begins a
// keyframe: an IDR slice (type 5) or an SPS (type 7), alone, inside a STAP-A,
// or as the first fragment of an FU-A.
func startsKeyframe(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}

	isKey := func(nalHeader byte) bool {
		naluType := nalHeader & 0x1f
		return naluType == 5 || naluType == 7
	}

	switch payload[0] & 0x1f {
	case 24: // STAP-A: 16-bit size, then the unit, repeated.
		for offset := 1; offset+2 < len(payload); {
			size := int(payload[offset])<<8 | int(payload[offset+1])
			if size == 0 || offset+2+size > len(payload) {
				return false
			}

			if isKey(payload[offset+2]) {
				return true
			}

			offset += 2 + size
		}

		return false
	case 28: // FU-A: the FU header carries the start bit and the unit type.
		return len(payload) > 1 && payload[1]&0x80 != 0 && isKey(payload[1])
	default:
		return isKey(payload[0])
	}
}

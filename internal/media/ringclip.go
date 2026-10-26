package media

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
	"github.com/pion/rtp"
)

var ErrRingClipNotFound = errors.New("media: ring clip is not available yet")

const (
	ringClipMaxDuration      = 15 * time.Second
	ringClipMinDuration      = time.Second
	ringClipMaxBytes         = 8 << 20
	ringClipPacketBufferSize = 256
	ringClipTimescale        = 90000
	ringClipMaxTicks         = int64(ringClipMaxDuration/time.Second) * ringClipTimescale
	// ringClipDefaultSampleDuration is 1/30 s, the length given to the only
	// sample of a one-sample clip.
	ringClipDefaultSampleDuration = 3000
	ringClipTemporaryPattern      = ".ringclip-*.mp4"
)

// RingClipRecorder records the first seconds of video of every doorbell ring
// and keeps the last clip of each entrypoint. The ring holder feeds it, so a
// ring is recorded whether or not a card is open. See
// docs/superpowers/specs/2026-10-26-ring-clip-design.md.
type RingClipRecorder struct {
	mu      sync.Mutex
	dir     string
	logger  *slog.Logger
	active  *RingClip
	onSaved func()
}

// RingClip is the recording of one ring. Its methods are safe on a nil clip.
type RingClip struct {
	recorder     *RingClipRecorder
	entrypointID string
	path         string
	packets      chan *rtp.Packet
	closeOnce    sync.Once
	closed       chan struct{}
	// done is closed once the clip is written or discarded.
	done chan struct{}
}

// NewRingClipRecorder keeps the clips below dir and removes the temporary files
// a crash left behind.
func NewRingClipRecorder(dir string, logger *slog.Logger) *RingClipRecorder {
	if logger == nil {
		logger = slog.Default()
	}

	recorder := &RingClipRecorder{dir: dir, logger: logger.With("component", "ring_clip")}
	recorder.removeLeftovers()

	return recorder
}

// SetOnSaved registers a notification emitted after a clip is published.
func (r *RingClipRecorder) SetOnSaved(callback func()) {
	if r == nil {
		return
	}

	r.mu.Lock()
	r.onSaved = callback
	r.mu.Unlock()
}

// Start begins the clip of a new ring and closes the one still open: the
// intercom carries one stream at a time.
func (r *RingClipRecorder) Start(entrypointID string) *RingClip {
	if r == nil {
		return nil
	}

	path, ok := r.path(entrypointID)
	if !ok {
		return nil
	}

	writer, err := newClipWriter()
	if err != nil {
		r.logger.Warn("start ring clip", "entrypoint_id", entrypointID, "error", err)
		return nil
	}

	clip := &RingClip{
		recorder: r, entrypointID: entrypointID, path: path,
		packets: make(chan *rtp.Packet, ringClipPacketBufferSize),
		closed:  make(chan struct{}), done: make(chan struct{}),
	}

	r.mu.Lock()
	previous := r.active
	r.active = clip
	r.mu.Unlock()

	previous.Close()

	go clip.run(writer)

	return clip
}

// Open returns the last clip of an entrypoint and the time it was recorded.
func (r *RingClipRecorder) Open(entrypointID string) (*os.File, time.Time, error) {
	if r == nil {
		return nil, time.Time{}, ErrRingClipNotFound
	}

	path, ok := r.path(entrypointID)
	if !ok {
		return nil, time.Time{}, ErrRingClipNotFound
	}

	file, err := os.Open(path) // #nosec G304 -- built from a validated entrypoint id below the data directory.
	if errors.Is(err, os.ErrNotExist) {
		return nil, time.Time{}, ErrRingClipNotFound
	}

	if err != nil {
		return nil, time.Time{}, fmt.Errorf("open ring clip: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, time.Time{}, fmt.Errorf("stat ring clip: %w", err)
	}

	return file, info.ModTime(), nil
}

// RecordedAt is when the last clip of an entrypoint started, zero when there is
// none. It is the file's modification time, so it survives a restart.
func (r *RingClipRecorder) RecordedAt(entrypointID string) time.Time {
	if r == nil {
		return time.Time{}
	}

	return modTime(r.path(entrypointID))
}

func (r *RingClipRecorder) path(entrypointID string) (string, bool) {
	name, ok := entrypointFileName(entrypointID)
	if !ok || r.dir == "" {
		return "", false
	}

	return filepath.Join(r.dir, "clips", "ring", name+".mp4"), true
}

func (r *RingClipRecorder) removeLeftovers() {
	if r.dir == "" {
		return
	}

	leftovers, err := filepath.Glob(filepath.Join(r.dir, "clips", "ring", ringClipTemporaryPattern))
	if err != nil {
		return
	}

	for _, leftover := range leftovers {
		if err := os.Remove(leftover); err != nil {
			r.logger.Warn("remove ring clip leftover", "path", leftover, "error", err)
		}
	}
}

// VideoRTP queues a copy of a video packet. It never blocks the fan-out: a
// packet that finds the queue full is dropped.
func (c *RingClip) VideoRTP(packet *rtp.Packet) {
	if c == nil || packet == nil {
		return
	}

	select {
	case <-c.closed:
		return
	default:
	}

	select {
	case c.packets <- packet.Clone():
	default:
	}
}

// Close ends the clip: what was recorded is written, or discarded when too
// short. It returns at once.
func (c *RingClip) Close() {
	if c != nil {
		c.closeOnce.Do(func() { close(c.closed) })
	}
}

func (c *RingClip) run(writer *clipWriter) {
	defer close(c.done)
	defer c.finish(writer)

	for {
		select {
		case packet := <-c.packets:
			if writer.push(packet) {
				return
			}
		case <-c.closed:
			// What was queued before the close still belongs to the clip.
			for {
				select {
				case packet := <-c.packets:
					if writer.push(packet) {
						return
					}
				default:
					return
				}
			}
		}
	}
}

func (c *RingClip) finish(writer *clipWriter) {
	c.Close()

	recorder := c.recorder

	recorder.mu.Lock()
	if recorder.active == c {
		recorder.active = nil
	}
	recorder.mu.Unlock()

	duration := writer.duration()
	if duration < ringClipMinDuration {
		recorder.logger.Debug("ring clip discarded; too short", "entrypoint_id", c.entrypointID, "duration", duration)
		return
	}

	data, err := writer.marshal()
	if err != nil {
		recorder.logger.Warn("encode ring clip", "entrypoint_id", c.entrypointID, "error", err)
		return
	}

	if err := writeFileAtomic(c.path, ringClipTemporaryPattern, data); err != nil {
		recorder.logger.Warn("publish ring clip", "entrypoint_id", c.entrypointID, "error", err)
		return
	}

	// The modification time is the start of the clip: RecordedAt reads it back.
	if err := os.Chtimes(c.path, writer.recordedAt, writer.recordedAt); err != nil {
		recorder.logger.Warn("date ring clip", "entrypoint_id", c.entrypointID, "error", err)
	}

	recorder.logger.Debug("ring clip saved", "entrypoint_id", c.entrypointID, "duration", duration, "bytes", len(data))

	recorder.mu.Lock()
	callback := recorder.onSaved
	recorder.mu.Unlock()

	if callback != nil {
		callback()
	}
}

// clipWriter turns the intercom's H.264 RTP into the samples of one clip. One
// goroutine uses it.
type clipWriter struct {
	decoder        *rtph264.Decoder
	sps, pps       []byte
	started        bool
	firstTimestamp uint32
	recordedAt     time.Time
	samples        []clipSample
	size           int
}

type clipSample struct {
	pts     int64
	payload []byte
	idr     bool
}

func newClipWriter() (*clipWriter, error) {
	decoder := &rtph264.Decoder{PacketizationMode: 1}
	if err := decoder.Init(); err != nil {
		return nil, fmt.Errorf("init H.264 depacketizer: %w", err)
	}

	return &clipWriter{decoder: decoder}, nil
}

// push adds a packet and reports whether the clip is complete.
func (w *clipWriter) push(packet *rtp.Packet) bool {
	accessUnit, err := w.decoder.Decode(packet)
	if err != nil {
		// More packets needed, a fragment without its start, or a broken access
		// unit: none ends the clip, the next access unit may be fine.
		return false
	}

	return w.accessUnit(packet.Timestamp, accessUnit)
}

// accessUnit adds one access unit and reports whether the clip is complete.
func (w *clipWriter) accessUnit(timestamp uint32, accessUnit [][]byte) bool {
	var sps, pps []byte

	idr := false
	nalus := make([][]byte, 0, len(accessUnit))

	for _, nalu := range accessUnit {
		if len(nalu) == 0 {
			continue
		}

		switch h264.NALUType(nalu[0] & 0x1f) {
		case h264.NALUTypeSPS:
			sps = nalu
		case h264.NALUTypePPS:
			pps = nalu
		case h264.NALUTypeAccessUnitDelimiter:
		case h264.NALUTypeIDR:
			idr = true
			nalus = append(nalus, nalu)
		default:
			nalus = append(nalus, nalu)
		}
	}

	if !w.started {
		// The SDP carries no sprop-parameter-sets: SPS and PPS come in band.
		if sps != nil {
			w.sps = bytes.Clone(sps)
		}

		if pps != nil {
			w.pps = bytes.Clone(pps)
		}

		if !idr || w.sps == nil || w.pps == nil {
			return false
		}

		w.started = true
		w.firstTimestamp = timestamp
		w.recordedAt = time.Now()
	} else if sps != nil && !bytes.Equal(sps, w.sps) {
		// The stream changed format: one track cannot carry both.
		return true
	}

	if len(nalus) == 0 {
		return false
	}

	// A signed difference keeps the order across a timestamp wrap.
	pts := int64(int32(timestamp - w.firstTimestamp)) // #nosec G115 -- intentional wrap-around.
	if pts >= ringClipMaxTicks {
		return true
	}

	if n := len(w.samples); n > 0 && pts <= w.samples[n-1].pts {
		return false
	}

	payload, err := h264.AVCC(nalus).Marshal()
	if err != nil {
		return false
	}

	if w.size+len(payload) > ringClipMaxBytes {
		return true
	}

	w.samples = append(w.samples, clipSample{pts: pts, payload: payload, idr: idr})
	w.size += len(payload)

	return false
}

// sampleDuration is the gap to the next sample; the last repeats the one before.
func (w *clipWriter) sampleDuration(i int) int64 {
	switch {
	case i+1 < len(w.samples):
		return w.samples[i+1].pts - w.samples[i].pts
	case i > 0:
		return w.samples[i].pts - w.samples[i-1].pts
	default:
		return ringClipDefaultSampleDuration
	}
}

func (w *clipWriter) duration() time.Duration {
	n := len(w.samples)
	if n == 0 {
		return 0
	}

	return ticksToDuration(w.samples[n-1].pts + w.sampleDuration(n-1))
}

func (w *clipWriter) marshal() ([]byte, error) {
	samples := make([]*pmp4.Sample, len(w.samples))
	for i, sample := range w.samples {
		samples[i] = &pmp4.Sample{
			Duration:        uint32(w.sampleDuration(i)), // #nosec G115 -- bounded by ringClipMaxTicks.
			IsNonSyncSample: !sample.idr,
			PayloadSize:     uint32(len(sample.payload)), // #nosec G115 -- bounded by ringClipMaxBytes.
			GetPayload:      func() ([]byte, error) { return sample.payload, nil },
		}
	}

	presentation := pmp4.Presentation{Tracks: []*pmp4.Track{{
		ID:        1,
		TimeScale: ringClipTimescale,
		Codec:     &codecs.H264{SPS: w.sps, PPS: w.pps},
		Samples:   samples,
	}}}

	var buffer bytes.Buffer
	if err := presentation.Marshal(&buffer); err != nil {
		return nil, fmt.Errorf("marshal ring clip: %w", err)
	}

	return buffer.Bytes(), nil
}

func ticksToDuration(ticks int64) time.Duration {
	return time.Duration(ticks) * time.Second / ringClipTimescale
}

package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"
)

var (
	ErrSnapshotUnavailable = errors.New("media: snapshot is unavailable")
	ErrSnapshotNotFound    = errors.New("media: snapshot is not available yet")
)

const (
	snapshotCaptureTimeout = 8 * time.Second
	// snapshotPacketBufferSize holds a whole keyframe: a dropped fragment breaks
	// it and the capture waits for the next one.
	snapshotPacketBufferSize = 256
	// snapshotMaxOutput bounds what the decoder may write back.
	snapshotMaxOutput = 4 << 20
)

// snapshotAccessUnitDelimiter ends the keyframe handed to the decoder, so the
// parser closes it without waiting for the next frame.
var snapshotAccessUnitDelimiter = []byte{0x09, 0xf0}

// SnapshotManager captures one JPEG from each source generation. It is passive:
// Arm never acquires a lease or sends intercom control commands.
//
// The capture depacketizes the H.264 stream itself and hands the decoder the
// first keyframe alone, as one Annex-B access unit on stdin, then reads the
// JPEG from stdout: no frame before the keyframe is decoded, nothing is lost to
// a decoder not yet listening, and the picture is read the moment it is
// written.
type SnapshotManager struct {
	mu         sync.Mutex
	logger     *slog.Logger
	dir        string
	runner     snapshotRunner
	active     *snapshotAttempt
	nextID     uint64
	onCaptured func()
}

type snapshotRunner interface {
	Start(context.Context) (snapshotProcess, error)
}

// snapshotProcess decodes one keyframe. It is started before the keyframe
// arrives, so the decoder is ready when it does.
type snapshotProcess interface {
	// Decode writes an Annex-B access unit, ends the input and returns what
	// the decoder wrote until it exited.
	Decode(ctx context.Context, accessUnit []byte) ([]byte, error)
	Close() error
}

type snapshotAttempt struct {
	manager      *SnapshotManager
	entrypointID string
	id           uint64
	ctx          context.Context
	cancel       context.CancelFunc
	packets      chan *rtp.Packet
	closeOnce    sync.Once
	// ring marks a capture from a stream born as a ringing preview: its picture
	// is also the doorbell picture.
	ring atomic.Bool
}

// NewSnapshotManager stores the latest valid JPEG for each entrypoint below dir.
func NewSnapshotManager(dir string, logger *slog.Logger) *SnapshotManager {
	if logger == nil {
		logger = slog.Default()
	}

	return newSnapshotManager(dir, logger, gstreamerSnapshotRunner{})
}

func newSnapshotManager(dir string, logger *slog.Logger, runner snapshotRunner) *SnapshotManager {
	if logger == nil {
		logger = slog.Default()
	}

	return &SnapshotManager{dir: dir, logger: logger, runner: runner}
}

// SetOnCaptured registers a notification emitted after a JPEG is atomically published.
func (s *SnapshotManager) SetOnCaptured(callback func()) {
	if s == nil {
		return
	}

	s.mu.Lock()
	s.onCaptured = callback
	s.mu.Unlock()
}

// Arm starts a background capture for a source that the coordinator already owns.
func (s *SnapshotManager) Arm(entrypointID string) *snapshotAttempt {
	if s == nil || entrypointID == "" || s.runner == nil {
		return nil
	}

	s.mu.Lock()
	if s.active != nil {
		s.active.Close()
	}

	s.nextID++
	ctx, cancel := context.WithCancel(context.Background()) // #nosec G118 -- snapshotAttempt.Close owns cancellation.
	attempt := &snapshotAttempt{
		manager: s, entrypointID: entrypointID, id: s.nextID,
		ctx: ctx, cancel: cancel, packets: make(chan *rtp.Packet, snapshotPacketBufferSize),
	}
	s.active = attempt
	s.mu.Unlock()

	go attempt.run()

	return attempt
}

// Latest returns the last successful JPEG without triggering media activity.
func (s *SnapshotManager) Latest(entrypointID string) ([]byte, error) {
	return s.read(entrypointID, s.path)
}

// LatestRing returns the picture taken at the last doorbell ring. A live view
// never writes it, so its time tells Home Assistant a new ring happened. See
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
func (s *SnapshotManager) LatestRing(entrypointID string) ([]byte, error) {
	return s.read(entrypointID, s.ringPath)
}

// UpdatedAt returns when the latest and the latest doorbell snapshot were
// taken, zero when there is none. It reads the files' modification times, so it
// survives a restart.
func (s *SnapshotManager) UpdatedAt(entrypointID string) (latest, ring time.Time) {
	if s == nil || s.dir == "" {
		return time.Time{}, time.Time{}
	}

	return modTime(s.path(entrypointID)), modTime(s.ringPath(entrypointID))
}

func (s *SnapshotManager) read(entrypointID string, locate func(string) (string, bool)) ([]byte, error) {
	if s == nil || s.dir == "" {
		return nil, ErrSnapshotUnavailable
	}

	path, ok := locate(entrypointID)
	if !ok {
		return nil, ErrSnapshotUnavailable
	}

	image, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrSnapshotNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("read latest snapshot: %w", err)
	}

	if _, ok := firstJPEG(image); !ok {
		return nil, ErrSnapshotNotFound
	}

	return image, nil
}

func modTime(path string, ok bool) time.Time {
	if !ok {
		return time.Time{}
	}

	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}

	return info.ModTime()
}

// Consume queues a best-effort RTP copy without blocking source delivery.
func (a *snapshotAttempt) Consume(packet *rtp.Packet) {
	if a == nil || packet == nil {
		return
	}

	select {
	case a.packets <- packet.Clone():
	default:
	}
}

func (a *snapshotAttempt) Close() {
	if a != nil {
		a.closeOnce.Do(a.cancel)
	}
}

// MarkRing makes this capture the doorbell picture too. It is called before the
// stream starts, so it is set before the first packet arrives.
func (a *snapshotAttempt) MarkRing() {
	if a != nil {
		a.ring.Store(true)
	}
}

func (a *snapshotAttempt) run() {
	defer a.finish()

	ctx, cancel := context.WithTimeout(a.ctx, snapshotCaptureTimeout)
	defer cancel()

	armed := time.Now()

	process, err := a.manager.runner.Start(ctx)
	if err != nil {
		a.manager.logger.Warn("start snapshot decoder", "entrypoint_id", a.entrypointID, "error", err)
		return
	}
	defer func() { _ = process.Close() }()

	keyframes := newSnapshotKeyframes()

	var firstPacket time.Time

	for {
		var packet *rtp.Packet

		select {
		case <-ctx.Done():
			return
		case packet = <-a.packets:
		}

		if firstPacket.IsZero() {
			firstPacket = time.Now()
		}

		accessUnit, ok := keyframes.push(packet)
		if !ok {
			continue
		}

		decodeStarted := time.Now()

		output, err := process.Decode(ctx, accessUnit)
		_ = process.Close()

		image, found := firstJPEG(output)
		if !found {
			if ctx.Err() != nil {
				return
			}

			// The decoder made no picture of this keyframe: a new decoder tries
			// the next one.
			a.manager.logger.Warn("snapshot decode produced no picture", "entrypoint_id", a.entrypointID, "error", err, "output_bytes", len(output))

			if process, err = a.manager.runner.Start(ctx); err != nil {
				a.manager.logger.Warn("restart snapshot decoder", "entrypoint_id", a.entrypointID, "error", err)
				return
			}

			continue
		}

		if !a.manager.publish(a, image) {
			return
		}

		a.manager.logger.Debug("snapshot captured", "entrypoint_id", a.entrypointID, "generation", a.id, "bytes", len(image), "ring", a.ring.Load(),
			"since_arm", time.Since(armed), "since_first_packet", time.Since(firstPacket), "decode", time.Since(decodeStarted),
			"skipped_access_units", keyframes.skipped)

		return
	}
}

// snapshotKeyframes depacketizes a video stream until its first keyframe. The
// SDP carries no sprop-parameter-sets, so SPS and PPS are taken in band.
type snapshotKeyframes struct {
	decoder  *rtph264.Decoder
	sps, pps []byte
	// skipped counts the access units seen before the keyframe.
	skipped int
}

func newSnapshotKeyframes() *snapshotKeyframes {
	decoder := &rtph264.Decoder{PacketizationMode: 1}
	_ = decoder.Init() // Init fails only on an unsupported packetization mode.

	return &snapshotKeyframes{decoder: decoder}
}

// push adds a packet and, once the first keyframe is complete, returns it with
// its parameter sets as one Annex-B access unit.
func (k *snapshotKeyframes) push(packet *rtp.Packet) ([]byte, bool) {
	nalus, err := k.decoder.Decode(packet)
	if err != nil {
		// More packets needed, or a broken access unit: the next one may do.
		return nil, false
	}

	idr := false
	slices := make([][]byte, 0, len(nalus))

	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}

		switch h264.NALUType(nalu[0] & 0x1f) {
		case h264.NALUTypeSPS:
			k.sps = bytes.Clone(nalu)
		case h264.NALUTypePPS:
			k.pps = bytes.Clone(nalu)
		case h264.NALUTypeAccessUnitDelimiter:
		case h264.NALUTypeIDR:
			idr = true
			slices = append(slices, nalu)
		default:
			slices = append(slices, nalu)
		}
	}

	if !idr || k.sps == nil || k.pps == nil {
		k.skipped++
		return nil, false
	}

	unit := append([][]byte{k.sps, k.pps}, slices...)

	encoded, err := h264.AnnexB(append(unit, snapshotAccessUnitDelimiter)).Marshal()
	if err != nil {
		k.skipped++
		return nil, false
	}

	return encoded, true
}

func (a *snapshotAttempt) finish() {
	a.manager.mu.Lock()
	if a.manager.active == a {
		a.manager.active = nil
	}
	a.manager.mu.Unlock()
}

func (s *SnapshotManager) publish(attempt *snapshotAttempt, image []byte) bool {
	s.mu.Lock()
	if s.active != attempt {
		s.mu.Unlock()
		return false
	}

	path, ok := s.path(attempt.entrypointID)
	if !ok {
		s.mu.Unlock()
		return false
	}

	if err := writeSnapshot(path, image); err != nil {
		s.mu.Unlock()
		s.logger.Warn("publish snapshot", "error", err)

		return false
	}

	// The doorbell picture has its own file, written only by a stream born as a
	// ringing preview, so a live view never looks like a new ring.
	if attempt.ring.Load() {
		if ringPath, ok := s.ringPath(attempt.entrypointID); ok {
			if err := writeSnapshot(ringPath, image); err != nil {
				s.logger.Warn("publish ring snapshot", "error", err)
			}
		}
	}

	callback := s.onCaptured
	s.mu.Unlock()

	if callback != nil {
		callback()
	}

	return true
}

// writeSnapshot publishes image at path atomically: a reader sees the old file
// or the new one, never a partial JPEG.
func writeSnapshot(path string, image []byte) error {
	return writeFileAtomic(path, ".snapshot-*.jpg", image)
}

// writeFileAtomic writes data to a temporary file named after pattern next to
// path and renames it over path.
func writeFileAtomic(path, pattern string, data []byte) error {
	return writeFileAtomicFunc(path, pattern, time.Time{}, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// writeFileAtomicFunc streams write into a temporary file named after pattern
// next to path and renames it over path. A non-zero mtime dates the temporary
// file before the rename, which keeps it, so a reader never sees the write time.
func writeFileAtomicFunc(path, pattern string, mtime time.Time, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}

	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck // removed after successful rename too

	if err := write(temporary); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}

	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}

	if !mtime.IsZero() {
		if err := os.Chtimes(temporaryPath, mtime, mtime); err != nil {
			return fmt.Errorf("date %s: %w", filepath.Base(path), err)
		}
	}

	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("rename %s: %w", filepath.Base(path), err)
	}

	return nil
}

func (s *SnapshotManager) path(entrypointID string) (string, bool) {
	name, ok := entrypointFileName(entrypointID)
	if !ok {
		return "", false
	}

	return filepath.Join(s.dir, "snapshots", name+".jpg"), true
}

// entrypointFileName is the entrypoint id as a file name, refused when it could
// leave its directory.
func entrypointFileName(entrypointID string) (string, bool) {
	entrypointID = strings.TrimSpace(entrypointID)
	if entrypointID == "" || entrypointID == "." || entrypointID == ".." || filepath.Base(entrypointID) != entrypointID || strings.ContainsRune(entrypointID, filepath.Separator) {
		return "", false
	}

	return entrypointID, true
}

// ringPath is the doorbell picture of an entrypoint. It lives in its own
// directory so no entrypoint name can collide with another's ring file.
func (s *SnapshotManager) ringPath(entrypointID string) (string, bool) {
	path, ok := s.path(entrypointID)
	if !ok {
		return "", false
	}

	return filepath.Join(filepath.Dir(path), "ring", filepath.Base(path)), true
}

func firstJPEG(data []byte) ([]byte, bool) {
	start := bytes.Index(data, []byte{0xff, 0xd8})
	if start < 0 {
		return nil, false
	}

	end := bytes.Index(data[start+2:], []byte{0xff, 0xd9})
	if end < 0 {
		return nil, false
	}

	return append([]byte(nil), data[start:start+end+4]...), true
}

type gstreamerSnapshotRunner struct{}

func (gstreamerSnapshotRunner) Start(ctx context.Context) (snapshotProcess, error) {
	command := exec.CommandContext(ctx, "/usr/bin/gst-launch-1.0", "-q", // #nosec G204 -- fixed executable and pipeline.
		"fdsrc", "fd=0", "!", "video/x-h264,stream-format=byte-stream", "!", "h264parse", "!", "imxvpudec", "!",
		"jpegenc", "quality=90", "!", "fdsink", "fd=1",
	)

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}

	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := command.Start(); err != nil {
		return nil, err
	}

	process := &gstreamerSnapshotProcess{command: command, stdin: stdin, done: make(chan struct{})}
	go func() {
		// Wait closes stdout, so it runs only after the output is read.
		process.output, process.readErr = io.ReadAll(io.LimitReader(stdout, snapshotMaxOutput))
		_ = command.Wait()

		close(process.done)
	}()

	return process, nil
}

type gstreamerSnapshotProcess struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	done    chan struct{}
	once    sync.Once
	// output and readErr are set before done is closed.
	output  []byte
	readErr error
}

// Decode ends the input after the access unit: the end of stream makes the
// decoder flush the picture and gst-launch exit.
func (p *gstreamerSnapshotProcess) Decode(ctx context.Context, accessUnit []byte) ([]byte, error) {
	_, writeErr := p.stdin.Write(accessUnit)
	closeErr := p.stdin.Close()

	select {
	case <-p.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return p.output, errors.Join(writeErr, closeErr, p.readErr)
}

func (p *gstreamerSnapshotProcess) Close() error {
	p.once.Do(func() {
		_ = p.stdin.Close()

		if p.command.Process != nil {
			_ = p.command.Process.Kill()
		}

		<-p.done
	})

	return nil
}

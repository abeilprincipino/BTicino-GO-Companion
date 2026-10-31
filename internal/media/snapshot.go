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
	// snapshotMaxStderr bounds what the decoder's errors may add to the log.
	snapshotMaxStderr = 4 << 10
	// snapshotMaxDecoders bounds the decoders one capture starts when one
	// exits without a picture.
	snapshotMaxDecoders = 3
)

// snapshotAccessUnitDelimiter ends each access unit handed to the decoder, so
// the parser closes it without waiting for the next one.
var snapshotAccessUnitDelimiter = []byte{0x09, 0xf0}

// SnapshotManager captures one JPEG from each source generation. It is passive:
// Arm never acquires a lease or sends intercom control commands.
//
// The capture depacketizes the H.264 stream itself and feeds the decoder from
// the first keyframe on, as Annex-B access units on stdin, then takes the first
// JPEG from stdout: no frame before the keyframe is decoded, nothing is lost to
// a decoder not yet listening, and the picture is read the moment it is
// written. The GStreamer decoder gets the keyframe alone and then the end of
// its input, which makes it drain the picture at once instead of holding it
// until more frames arrive.
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

// snapshotProcess is a decoder turning access units into JPEGs. It is started
// before the keyframe arrives, so it is ready when it does.
type snapshotProcess interface {
	// Write feeds the decoder an Annex-B access unit.
	Write(accessUnit []byte) error
	// Picture yields the first JPEG the decoder writes and is closed when the
	// decoder exits.
	Picture() <-chan []byte
	// Err says why the decoder exited. It is set once Picture is closed.
	Err() error
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
	// done is set when the capture ends. The source keeps calling Consume for
	// the whole stream, and nothing reads the packets any more.
	done atomic.Bool
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
	if a == nil || packet == nil || a.done.Load() {
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

	accessUnits := newSnapshotAccessUnits()
	decoders := 1
	fed := 0

	var firstPacket, decodeStarted time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case image, ok := <-process.Picture():
			if ok {
				if a.manager.publish(a, image) {
					a.manager.logger.Debug("snapshot captured", "entrypoint_id", a.entrypointID, "generation", a.id, "bytes", len(image), "ring", a.ring.Load(),
						"since_arm", time.Since(armed), "since_first_packet", time.Since(firstPacket), "decode", time.Since(decodeStarted),
						"fed_access_units", fed, "skipped_access_units", accessUnits.skipped,
						"nal_types_until_keyframe", strings.Join(accessUnits.skippedTypes, " "), "broken_packets", accessUnits.broken)
				}

				return
			}

			a.manager.logger.Warn("snapshot decoder exited without a picture", "entrypoint_id", a.entrypointID, "decoder", decoders,
				"fed_access_units", fed, "error", process.Err())

			// A decoder that fails before any input will fail again.
			if fed == 0 || decoders >= snapshotMaxDecoders || ctx.Err() != nil {
				return
			}

			_ = process.Close()

			if process, err = a.manager.runner.Start(ctx); err != nil {
				a.manager.logger.Warn("restart snapshot decoder", "entrypoint_id", a.entrypointID, "error", err)
				return
			}

			decoders++
			fed = 0
			decodeStarted = time.Time{}

			accessUnits.restart()
		case packet := <-a.packets:
			if firstPacket.IsZero() {
				firstPacket = time.Now()
			}

			accessUnit, ok := accessUnits.push(packet)
			if !ok {
				continue
			}

			if decodeStarted.IsZero() {
				decodeStarted = time.Now()
			}

			fed++

			// A failed write means the decoder is gone: Picture says why.
			_ = process.Write(accessUnit)
		}
	}
}

// snapshotAccessUnits depacketizes a video stream into Annex-B access units
// from its first keyframe on. The SDP carries no sprop-parameter-sets, so SPS
// and PPS are taken in band and put before every keyframe.
type snapshotAccessUnits struct {
	decoder  *rtph264.Decoder
	sps, pps []byte
	started  bool
	// skipped counts the access units seen before the first keyframe;
	// skippedTypes lists the NAL unit types of the first of them.
	skipped      int
	skippedTypes []string
	// broken counts the packets the depacketizer refused, other than the ones
	// that only wait for more packets.
	broken int
}

const snapshotMaxSkippedTypes = 32

func newSnapshotAccessUnits() *snapshotAccessUnits {
	decoder := &rtph264.Decoder{PacketizationMode: 1}
	_ = decoder.Init() // Init fails only on an unsupported packetization mode.

	return &snapshotAccessUnits{decoder: decoder}
}

// push adds a packet and returns the access unit it completes, once a keyframe
// with its parameter sets has started the stream.
func (u *snapshotAccessUnits) push(packet *rtp.Packet) ([]byte, bool) {
	nalus, err := u.decoder.Decode(packet)
	if err != nil {
		// More packets needed, or a broken access unit: the next one may do.
		if !errors.Is(err, rtph264.ErrMorePacketsNeeded) {
			u.broken++
		}

		return nil, false
	}

	idr := false
	slices := make([][]byte, 0, len(nalus))
	types := make([]string, 0, len(nalus))

	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}

		types = append(types, fmt.Sprint(nalu[0]&0x1f))

		switch h264.NALUType(nalu[0] & 0x1f) {
		case h264.NALUTypeSPS:
			u.sps = bytes.Clone(nalu)
		case h264.NALUTypePPS:
			u.pps = bytes.Clone(nalu)
		case h264.NALUTypeAccessUnitDelimiter:
		case h264.NALUTypeIDR:
			idr = true
			slices = append(slices, nalu)
		default:
			slices = append(slices, nalu)
		}
	}

	if !u.started && len(u.skippedTypes) < snapshotMaxSkippedTypes {
		u.skippedTypes = append(u.skippedTypes, strings.Join(types, "+"))
	}

	if len(slices) == 0 {
		return nil, false
	}

	if !u.started && (!idr || u.sps == nil || u.pps == nil) {
		u.skipped++
		return nil, false
	}

	unit := slices
	if idr {
		unit = append([][]byte{u.sps, u.pps}, slices...)
	}

	encoded, err := h264.AnnexB(append(unit, snapshotAccessUnitDelimiter)).Marshal()
	if err != nil {
		return nil, false
	}

	u.started = true

	return encoded, true
}

// restart waits for the next keyframe, for a new decoder.
func (u *snapshotAccessUnits) restart() {
	u.started = false
}

func (a *snapshotAttempt) finish() {
	a.done.Store(true)

	// Release the copies still queued: the attempt lives as long as the stream.
	for draining := true; draining; {
		select {
		case <-a.packets:
		default:
			draining = false
		}
	}

	a.manager.mu.Lock()
	if a.manager.active == a {
		a.manager.active = nil
	}
	a.manager.mu.Unlock()
}

func (s *SnapshotManager) publish(attempt *snapshotAttempt, image []byte) bool {
	s.mu.Lock()
	// A closed attempt stays active until its run returns, and its picture may
	// be ready together with the cancellation.
	if s.active != attempt || attempt.ctx.Err() != nil {
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
		// No caps filter after fdsrc: GStreamer 1.12 on the intercom refuses
		// partial caps there ("Filter caps do not completely specify the
		// output format"), and h264parse detects the byte-stream itself.
		//
		// The JPEG is encoded on the CPU, not with imxvpuenc_mjpeg: idle, the
		// VPU encoder is ~20 ms quicker, but during a ring the intercom opens
		// its own decoders at the same instant, and four rings took ~140 ms
		// against ~115 with jpegenc. Quality 80 keeps a 640x480 picture at
		// ~40 KB instead of ~58 at 90. The VPU library prints its logs on
		// stdout after the picture; readFirstJPEG skips them.
		"fdsrc", "fd=0", "!", "h264parse", "!", "imxvpudec", "!",
		"jpegenc", "quality=80", "!", "fdsink", "fd=1",
	)

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}

	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}

	stderr := &cappedBuffer{max: snapshotMaxStderr}
	command.Stderr = stderr

	if err := command.Start(); err != nil {
		return nil, err
	}

	process := &gstreamerSnapshotProcess{command: command, stdin: stdin, picture: make(chan []byte, 1)}
	go func() {
		image, readErr := readFirstJPEG(stdout)
		if image != nil {
			process.picture <- image
		}

		// Wait closes stdout, so it runs only after the output is read. After a
		// picture it returns once Close kills the decoder.
		waitErr := command.Wait()

		process.err = errors.Join(readErr, waitErr)
		if message := strings.TrimSpace(stderr.String()); message != "" {
			process.err = errors.Join(process.err, errors.New(message))
		}

		close(process.picture)
	}()

	return process, nil
}

type gstreamerSnapshotProcess struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	picture chan []byte
	once    sync.Once
	// ended is set once the keyframe is written and stdin closed.
	ended bool
	// err is set before picture is closed.
	err error
}

var errSnapshotInputEnded = errors.New("snapshot decoder input already ended")

// Write feeds the decoder the keyframe and closes its input: the end of stream
// makes imxvpudec drain the picture now rather than after more frames. Writes
// after the first fail, and the capture ignores them. A decoder that exits
// without a picture is replaced on the next keyframe.
func (p *gstreamerSnapshotProcess) Write(accessUnit []byte) error {
	if p.ended {
		return errSnapshotInputEnded
	}

	p.ended = true

	_, err := p.stdin.Write(accessUnit)

	return errors.Join(err, p.stdin.Close())
}

func (p *gstreamerSnapshotProcess) Picture() <-chan []byte { return p.picture }

func (p *gstreamerSnapshotProcess) Err() error { return p.err }

func (p *gstreamerSnapshotProcess) Close() error {
	p.once.Do(func() {
		_ = p.stdin.Close()

		if p.command.Process != nil {
			_ = p.command.Process.Kill()
		}

		for range p.picture {
		}
	})

	return nil
}

// readFirstJPEG reads the decoder's output until it holds a whole JPEG and
// returns it without waiting for the decoder to exit. It returns nil when the
// output ends first.
func readFirstJPEG(r io.Reader) ([]byte, error) {
	var data []byte

	chunk := make([]byte, 32<<10)

	for len(data) < snapshotMaxOutput {
		n, err := r.Read(chunk)
		data = append(data, chunk[:n]...)

		if image, ok := firstJPEG(data); ok {
			return image, nil
		}

		if errors.Is(err, io.EOF) {
			return nil, nil
		}

		if err != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf("decoder wrote %d bytes without a picture", len(data))
}

// cappedBuffer keeps the first max bytes written to it.
type cappedBuffer struct {
	bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}

	return len(p), nil
}

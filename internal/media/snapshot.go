package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
)

var (
	ErrSnapshotUnavailable = errors.New("media: snapshot is unavailable")
	ErrSnapshotNotFound    = errors.New("media: snapshot is not available yet")
)

const (
	snapshotRTPAddress       = "127.0.0.1:51074"
	snapshotCaptureTimeout   = 8 * time.Second
	snapshotPollInterval     = 50 * time.Millisecond
	snapshotPacketBufferSize = 64
)

// SnapshotManager captures one JPEG from each source generation. It is passive:
// Arm never acquires a lease or sends intercom control commands.
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
	Start(context.Context, string) (snapshotProcess, error)
}

type snapshotProcess interface {
	Close() error
}

type snapshotAttempt struct {
	manager      *SnapshotManager
	entrypointID string
	id           uint64
	ctx          context.Context
	cancel       context.CancelFunc
	packets      chan []byte
	closeOnce    sync.Once
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
		ctx: ctx, cancel: cancel, packets: make(chan []byte, snapshotPacketBufferSize),
	}
	s.active = attempt
	s.mu.Unlock()

	go attempt.run()

	return attempt
}

// Latest returns the last successful JPEG without triggering media activity.
func (s *SnapshotManager) Latest(entrypointID string) ([]byte, error) {
	if s == nil || s.dir == "" {
		return nil, ErrSnapshotUnavailable
	}

	path, ok := s.path(entrypointID)
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

// Consume queues a best-effort RTP copy without blocking source delivery.
func (a *snapshotAttempt) Consume(packet *rtp.Packet) {
	if a == nil || packet == nil {
		return
	}

	raw, err := packet.Marshal()
	if err != nil {
		return
	}

	select {
	case a.packets <- raw:
	default:
	}
}

func (a *snapshotAttempt) Close() {
	if a != nil {
		a.closeOnce.Do(a.cancel)
	}
}

func (a *snapshotAttempt) run() {
	defer a.finish()

	output, err := os.CreateTemp("", "bticino-snapshot-*.mjpeg")
	if err != nil {
		a.manager.logger.Warn("create snapshot output", "entrypoint_id", a.entrypointID, "error", err)
		return
	}

	outputPath := output.Name()
	if err := output.Close(); err != nil {
		a.manager.logger.Warn("close snapshot output", "entrypoint_id", a.entrypointID, "error", err)
		return
	}
	defer os.Remove(outputPath) //nolint:errcheck // temporary capture cleanup

	ctx, cancel := context.WithTimeout(a.ctx, snapshotCaptureTimeout)
	defer cancel()

	process, err := a.manager.runner.Start(ctx, outputPath)
	if err != nil {
		a.manager.logger.Warn("start snapshot capture", "entrypoint_id", a.entrypointID, "error", err)
		return
	}
	defer process.Close() //nolint:errcheck // capture process cleanup

	conn, err := net.DialUDP("udp4", nil, mustResolveSnapshotAddress())
	if err != nil {
		a.manager.logger.Warn("connect snapshot RTP", "entrypoint_id", a.entrypointID, "error", err)
		return
	}
	defer conn.Close() //nolint:errcheck // loopback socket cleanup

	poll := time.NewTicker(snapshotPollInterval)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case packet := <-a.packets:
			_, _ = conn.Write(packet)
		case <-poll.C:
			data, err := os.ReadFile(outputPath)
			if err != nil {
				continue
			}

			image, ok := firstJPEG(data)
			if !ok || !a.manager.publish(a, image) {
				continue
			}

			a.manager.logger.Debug("snapshot captured", "entrypoint_id", a.entrypointID, "generation", a.id, "bytes", len(image))

			return
		}
	}
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

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		s.mu.Unlock()
		s.logger.Warn("create snapshot directory", "error", err)

		return false
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*.jpg")
	if err != nil {
		s.mu.Unlock()
		s.logger.Warn("create snapshot file", "error", err)

		return false
	}

	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck // removed after successful rename too

	if _, err := temporary.Write(image); err != nil {
		_ = temporary.Close()
		s.mu.Unlock()
		s.logger.Warn("write snapshot", "error", err)

		return false
	}

	if err := temporary.Close(); err != nil {
		s.mu.Unlock()
		s.logger.Warn("close snapshot", "error", err)

		return false
	}

	if err := os.Rename(temporaryPath, path); err != nil {
		s.mu.Unlock()
		s.logger.Warn("publish snapshot", "error", err)

		return false
	}

	callback := s.onCaptured
	s.mu.Unlock()

	if callback != nil {
		callback()
	}

	return true
}

func (s *SnapshotManager) path(entrypointID string) (string, bool) {
	entrypointID = strings.TrimSpace(entrypointID)
	if entrypointID == "" || entrypointID == "." || entrypointID == ".." || filepath.Base(entrypointID) != entrypointID || strings.ContainsRune(entrypointID, filepath.Separator) {
		return "", false
	}

	return filepath.Join(s.dir, "snapshots", entrypointID+".jpg"), true
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

func mustResolveSnapshotAddress() *net.UDPAddr {
	address, _ := net.ResolveUDPAddr("udp4", snapshotRTPAddress)
	return address
}

type gstreamerSnapshotRunner struct{}

func (gstreamerSnapshotRunner) Start(ctx context.Context, outputPath string) (snapshotProcess, error) {
	// outputPath comes from os.CreateTemp and is passed as an argument, not through a shell.
	command := exec.CommandContext(ctx, "/usr/bin/gst-launch-1.0", "-q", // #nosec G204 -- fixed executable and pipeline; generated output path is an argument.
		"udpsrc", "address=127.0.0.1", "port=51074", "caps=application/x-rtp,media=video,encoding-name=H264,clock-rate=90000,payload=96",
		"!", "rtph264depay", "!", "h264parse", "!", "imxvpudec", "!", "jpegenc", "quality=90", "!", "filesink", "location="+outputPath,
	)
	if err := command.Start(); err != nil {
		return nil, err
	}

	process := &gstreamerSnapshotProcess{command: command, done: make(chan struct{})}
	go func() {
		_ = command.Wait()

		close(process.done)
	}()

	return process, nil
}

type gstreamerSnapshotProcess struct {
	command *exec.Cmd
	done    chan struct{}
	once    sync.Once
}

func (p *gstreamerSnapshotProcess) Close() error {
	p.once.Do(func() {
		if p.command.Process != nil {
			_ = p.command.Process.Kill()
		}

		<-p.done
	})

	return nil
}

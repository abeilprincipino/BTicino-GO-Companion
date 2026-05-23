package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/services/media"
)

const (
	defaultCaptureTimeout = 10 * time.Second
)

var (
	ErrEntrypointNotFound      = errors.New("entrypoint not found")
	ErrCapabilityNotEnabled    = errors.New("entrypoint capability not enabled")
	ErrSnapshotBusy            = errors.New("snapshot capture already in progress")
	ErrSnapshotUnavailable     = errors.New("snapshot service unavailable")
	ErrSnapshotTimeout         = errors.New("snapshot capture timed out")
	ErrSnapshotNotFound        = errors.New("snapshot not found")
	ErrActiveEntrypointBlocked = errors.New("another entrypoint stream is already active")
)

type streamDriver interface {
	StartForEntrypoint(ctx context.Context, entrypointID string, devAddr string) error
	StopForEntrypoint(ctx context.Context, entrypointID string) error
	Snapshot() media.Snapshot
}

type mirrorDriver interface {
	BeginSnapshotMirror() (int, func(), error)
}

type Service struct {
	stream         streamDriver
	mirror         mirrorDriver
	logger         *log.Logger
	snapshotsDir   string
	entrypoints    map[string]entrypoint.Model
	captureTimeout time.Duration
	gstBinary      string

	captureMu sync.Mutex
}

func New(cfg config.Config, stream streamDriver, mirror mirrorDriver, logger *log.Logger) *Service {
	index := make(map[string]entrypoint.Model, len(cfg.Entrypoints))
	for _, ep := range cfg.Entrypoints {
		id := strings.TrimSpace(ep.ID)
		if id == "" {
			continue
		}
		index[id] = ep
	}
	return &Service{
		stream:         stream,
		mirror:         mirror,
		logger:         logger,
		snapshotsDir:   filepath.Join(cfg.DataDir, "media", "snapshots"),
		entrypoints:    index,
		captureTimeout: defaultCaptureTimeout,
		gstBinary:      "gst-launch-1.0",
	}
}

func (s *Service) Capture(ctx context.Context, entrypointID string) ([]byte, error) {
	ep, err := s.entrypoint(entrypointID)
	if err != nil {
		return nil, err
	}
	if s.stream == nil || s.mirror == nil {
		return nil, ErrSnapshotUnavailable
	}
	if !s.captureMu.TryLock() {
		return nil, ErrSnapshotBusy
	}
	defer s.captureMu.Unlock()

	if err := os.MkdirAll(s.snapshotsDir, 0o755); err != nil {
		return nil, fmt.Errorf("prepare snapshot directory: %w", err)
	}

	before := s.stream.Snapshot()
	if before.StreamActive && before.ActiveEntrypoint != "" && before.ActiveEntrypoint != ep.ID {
		return nil, ErrActiveEntrypointBlocked
	}

	startedBySnapshot := false
	if !before.StreamActive {
		if err := s.stream.StartForEntrypoint(ctx, ep.ID, ep.DevAddr); err != nil {
			return nil, err
		}
		startedBySnapshot = true
	}
	if startedBySnapshot {
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.stream.StopForEntrypoint(stopCtx, ep.ID); err != nil {
				s.logf("snapshot stream stop warning entrypoint=%s err=%v", ep.ID, err)
			}
		}()
	}

	port, stopMirror, err := s.mirror.BeginSnapshotMirror()
	if err != nil {
		return nil, err
	}
	defer stopMirror()

	finalPath := s.pathForEntrypoint(ep.ID)
	tmpPath := finalPath + ".tmp"
	_ = os.Remove(tmpPath)

	captureCtx, cancel := context.WithTimeout(ctx, s.captureTimeout)
	defer cancel()
	if err := s.runCapturePipeline(captureCtx, port, tmpPath); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(captureCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrSnapshotTimeout
		}
		return nil, err
	}

	info, err := os.Stat(tmpPath)
	if err != nil || info.Size() <= 0 {
		return nil, ErrSnapshotTimeout
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		return nil, fmt.Errorf("publish snapshot: %w", err)
	}
	image, err := os.ReadFile(finalPath)
	if err != nil {
		return nil, err
	}
	return image, nil
}

func (s *Service) Latest(entrypointID string) (string, error) {
	ep, err := s.entrypoint(entrypointID)
	if err != nil {
		return "", err
	}
	path := s.pathForEntrypoint(ep.ID)
	info, err := os.Stat(path)
	if err != nil || info.Size() <= 0 {
		return "", ErrSnapshotNotFound
	}
	return path, nil
}

func (s *Service) runCapturePipeline(ctx context.Context, port int, outputPath string) error {
	caps := "application/x-rtp,media=video,encoding-name=H264,payload=96,clock-rate=90000"
	args := []string{
		"-q",
		"udpsrc", fmt.Sprintf("port=%d", port), fmt.Sprintf("caps=%s", caps),
		"!",
		"rtph264depay",
		"!",
		"h264parse", "config-interval=-1",
		"!",
		"imxvpudec",
		"!",
		"videoconvert",
		"!",
		"jpegenc", "quality=90",
		"!",
		"filesink", "sync=false", fmt.Sprintf("location=%s", outputPath),
	}
	cmd := exec.CommandContext(ctx, s.gstBinary, args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start snapshot pipeline: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	poll := time.NewTicker(30 * time.Millisecond)
	defer poll.Stop()

	captured := false
	for {
		select {
		case err := <-waitCh:
			if captured {
				ok, _ := isCompleteJPEG(outputPath)
				if ok {
					return nil
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return fmt.Errorf("snapshot pipeline failed: %w output=%s", err, strings.TrimSpace(output.String()))
			}
			ok, checkErr := isCompleteJPEG(outputPath)
			if checkErr == nil && ok {
				return nil
			}
			return fmt.Errorf("snapshot pipeline ended before jpeg completion output=%s", strings.TrimSpace(output.String()))
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-waitCh
			return ctx.Err()
		case <-poll.C:
			ok, err := isCompleteJPEG(outputPath)
			if err != nil || !ok {
				continue
			}
			captured = true
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}
}

func (s *Service) pathForEntrypoint(entrypointID string) string {
	return filepath.Join(s.snapshotsDir, fmt.Sprintf("%s.jpg", entrypointID))
}

func (s *Service) entrypoint(id string) (entrypoint.Model, error) {
	normalized := strings.TrimSpace(id)
	if normalized == "" {
		return entrypoint.Model{}, ErrEntrypointNotFound
	}
	ep, ok := s.entrypoints[normalized]
	if !ok {
		return entrypoint.Model{}, ErrEntrypointNotFound
	}
	if !ep.HasStream {
		return entrypoint.Model{}, ErrCapabilityNotEnabled
	}
	return ep, nil
}

func (s *Service) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}

func isCompleteJPEG(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() < 4 {
		return false, nil
	}

	var start [2]byte
	if _, err := io.ReadFull(f, start[:]); err != nil {
		return false, err
	}
	if start[0] != 0xFF || start[1] != 0xD8 {
		return false, nil
	}

	if _, err := f.Seek(-2, io.SeekEnd); err != nil {
		return false, err
	}
	var end [2]byte
	if _, err := io.ReadFull(f, end[:]); err != nil {
		return false, err
	}
	if end[0] != 0xFF || end[1] != 0xD9 {
		return false, nil
	}
	return true, nil
}

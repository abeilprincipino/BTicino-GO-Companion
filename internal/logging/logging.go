package logging

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	Path       = "/tmp/companion.log"
	maxSize    = 1 << 20
	maxBackups = 3
)

type Runtime struct {
	Logger *slog.Logger
	level  *slog.LevelVar
	file   *rotatingFile
}

func New(level string) (*Runtime, error) {
	parsed, err := parseLevel(level)
	if err != nil {
		return nil, err
	}

	file, err := newRotatingFile(Path)
	if err != nil {
		return nil, err
	}

	levelVar := &slog.LevelVar{}
	levelVar.Set(parsed)
	handler := slog.NewJSONHandler(io.MultiWriter(os.Stdout, file), &slog.HandlerOptions{Level: levelVar})

	return &Runtime{Logger: slog.New(handler), level: levelVar, file: file}, nil
}

func (r *Runtime) SetLevel(level string) error {
	parsed, err := parseLevel(level)
	if err != nil {
		return err
	}

	r.level.Set(parsed)
	return nil
}

func (r *Runtime) Close() error {
	if r == nil || r.file == nil {
		return nil
	}

	return r.file.Close()
}

func HTTP(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		response := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(response, r)

		route := r.Pattern
		if route == "" {
			route = r.URL.Path
		}
		logger.InfoContext(r.Context(), "http request", "method", r.Method, "route", route, "status", response.status, "duration", time.Since(started), "remote_addr", r.RemoteAddr)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *responseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}

	return hijacker.Hijack()
}

func parseLevel(value string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(value)); err != nil {
		return 0, fmt.Errorf("parse log level: %w", err)
	}

	return level, nil
}

type rotatingFile struct {
	mu   sync.Mutex
	path string
	file *os.File
}

func newRotatingFile(path string) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	return &rotatingFile{path: path, file: file}, nil
}

func (r *rotatingFile) Write(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	info, err := r.file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat log file: %w", err)
	}
	if info.Size()+int64(len(data)) > maxSize {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}

	return r.file.Write(data)
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		return nil
	}

	err := r.file.Close()
	r.file = nil
	return err
}

func (r *rotatingFile) rotate() error {
	if err := r.file.Close(); err != nil {
		return fmt.Errorf("close log before rotation: %w", err)
	}

	for index := maxBackups; index >= 1; index-- {
		source := r.path + "." + strconv.Itoa(index)
		destination := r.path + "." + strconv.Itoa(index+1)
		if index == maxBackups {
			if err := os.Remove(source); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove old log: %w", err)
			}
			continue
		}
		if err := os.Rename(source, destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate log: %w", err)
		}
	}

	if err := os.Rename(r.path, r.path+".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("archive log: %w", err)
	}

	file, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open rotated log: %w", err)
	}

	r.file = file
	return nil
}

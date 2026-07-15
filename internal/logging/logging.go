package logging

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	handler := newHumanHandler(file, levelVar)

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
		if r.URL.Path == "/webui/api/logs" {
			return
		}

		attributes := []any{"method", r.Method, "route", route, "status", response.status, "duration", time.Since(started), "remote_addr", r.RemoteAddr}
		if response.status >= http.StatusBadRequest {
			if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
				logger.DebugContext(r.Context(), "http request rejected", attributes...)
				return
			}
			logger.WarnContext(r.Context(), "http request failed", attributes...)
			return
		}

		logger.DebugContext(r.Context(), "http request", attributes...)
	})
}

type humanHandler struct {
	output io.Writer
	level  slog.Leveler
	attrs  []slog.Attr
	groups []string
}

func newHumanHandler(output io.Writer, level slog.Leveler) *humanHandler {
	return &humanHandler{output: output, level: level}
}

func (h *humanHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *humanHandler) Handle(_ context.Context, record slog.Record) error {
	var line strings.Builder
	line.WriteString(record.Time.Local().Format("2006-01-02 15:04:05"))
	line.WriteByte(' ')
	line.WriteString(shortLevel(record.Level))
	line.WriteByte(' ')
	line.WriteString(record.Message)

	for _, attr := range h.attrs {
		appendAttr(&line, h.groups, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		appendAttr(&line, h.groups, attr)
		return true
	})

	_, err := io.WriteString(h.output, line.String()+"\n")
	return err
}

func (h *humanHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &next
}

func (h *humanHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.groups = append(append([]string{}, h.groups...), name)
	return &next
}

func shortLevel(level slog.Level) string {
	switch {
	case level <= slog.LevelDebug:
		return "DBG"
	case level < slog.LevelWarn:
		return "INF"
	case level < slog.LevelError:
		return "WRN"
	default:
		return "ERR"
	}
}

func appendAttr(line *strings.Builder, groups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return
	}
	if attr.Value.Kind() == slog.KindGroup {
		for _, nested := range attr.Value.Group() {
			appendAttr(line, append(groups, attr.Key), nested)
		}
		return
	}
	key := attr.Key
	if len(groups) > 0 {
		key = strings.Join(groups, ".") + "." + key
	}
	line.WriteByte(' ')
	line.WriteString(key)
	line.WriteByte('=')
	line.WriteString(formatValue(attr.Value))
}

func formatValue(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		return quoteIfNeeded(value.String())
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindTime:
		return value.Time().Local().Format("2006-01-02 15:04:05")
	case slog.KindBool:
		return strconv.FormatBool(value.Bool())
	case slog.KindInt64:
		return strconv.FormatInt(value.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(value.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(value.Float64(), 'f', -1, 64)
	default:
		return quoteIfNeeded(fmt.Sprint(value.Any()))
	}
}

func quoteIfNeeded(value string) string {
	if value == "" || strings.ContainsAny(value, " \t\n\r\"") {
		return strconv.Quote(value)
	}
	return value
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

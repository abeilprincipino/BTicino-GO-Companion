package logging

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRotatingFileRotatesAndPreservesPermissions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "companion.log")
	file, err := newRotatingFile(path)
	if err != nil {
		t.Fatalf("new rotating file: %v", err)
	}
	defer file.Close()

	if _, err := file.file.WriteString(strings.Repeat("a", maxSize)); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	if _, err := file.Write([]byte("next\n")); err != nil {
		t.Fatalf("rotate log: %v", err)
	}

	archived, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read archived log: %v", err)
	}
	if len(archived) != maxSize {
		t.Fatalf("archive size = %d, want %d", len(archived), maxSize)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active log: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %o, want 600", info.Mode().Perm())
	}
}

func TestHumanHandlerFormatsReadableLine(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	level := &slog.LevelVar{}
	level.Set(slog.LevelDebug)
	handler := newHumanHandler(&output, level)
	record := slog.NewRecord(time.Date(2026, time.July, 15, 13, 49, 48, 0, time.Local), slog.LevelInfo, "api listening", 0)
	record.AddAttrs(slog.String("addr", ":8080"), slog.String("error", "unexpected reply"))
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("handle record: %v", err)
	}

	if got, want := output.String(), "2026-07-15 13:49:48 INF api listening addr=:8080 error=\"unexpected reply\"\n"; got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

func TestParseLevel(t *testing.T) {
	t.Parallel()

	if _, err := parseLevel("info"); err != nil {
		t.Fatalf("parse info: %v", err)
	}
	if _, err := parseLevel("invalid"); err == nil {
		t.Fatal("invalid level succeeded")
	}
}

func TestHTTPSuppressesWebUIAPIRequests(t *testing.T) {
	var output bytes.Buffer
	level := &slog.LevelVar{}
	level.Set(slog.LevelDebug)
	handler := HTTP(slog.New(newHumanHandler(&output, level)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/webui/api/status", nil))

	if output.Len() != 0 {
		t.Fatalf("web UI API request logged: %q", output.String())
	}
}

func TestHTTPSuppressesSuccessfulHealthChecks(t *testing.T) {
	var output bytes.Buffer
	level := &slog.LevelVar{}
	level.Set(slog.LevelDebug)
	handler := HTTP(slog.New(newHumanHandler(&output, level)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v3/health", nil))

	if output.Len() != 0 {
		t.Fatalf("successful health check logged: %q", output.String())
	}
}

func TestHTTPSuppressesSuccessfulRequests(t *testing.T) {
	var output bytes.Buffer
	level := &slog.LevelVar{}
	level.Set(slog.LevelDebug)
	handler := HTTP(slog.New(newHumanHandler(&output, level)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v3/state", nil))

	if output.Len() != 0 {
		t.Fatalf("successful request logged: %q", output.String())
	}
}

func TestHTTPLogsWebUIFailures(t *testing.T) {
	var output bytes.Buffer
	level := &slog.LevelVar{}
	level.Set(slog.LevelDebug)
	handler := HTTP(slog.New(newHumanHandler(&output, level)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/webui/api/status", nil))

	if !strings.Contains(output.String(), "http request failed") {
		t.Fatalf("web UI failure was not logged: %q", output.String())
	}
}

func TestHTTPLogsFailedHealthChecks(t *testing.T) {
	var output bytes.Buffer
	level := &slog.LevelVar{}
	level.Set(slog.LevelDebug)
	handler := HTTP(slog.New(newHumanHandler(&output, level)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v3/health", nil))

	if !strings.Contains(output.String(), "http request failed") {
		t.Fatalf("failed health check was not logged: %q", output.String())
	}
}

package app

import (
	"bticino-go-companion/internal/config"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenConfigCreatesThenReusesConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	detectCalls := 0
	detect := func() (config.Metadata, error) {
		detectCalls++
		return config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"}, nil
	}

	store, created, err := openConfig(path, detect)
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if store.Snapshot().Companion.DeviceID != "c300x-001122334455" {
		t.Fatalf("device id = %q", store.Snapshot().Companion.DeviceID)
	}

	store, created, err = openConfig(path, detect)
	if err != nil {
		t.Fatalf("reuse config: %v", err)
	}
	if created {
		t.Fatal("created = true, want false")
	}
	if detectCalls != 2 {
		t.Fatalf("metadata detection calls = %d, want 2", detectCalls)
	}
	if store.Snapshot().Companion.DeviceID != "c300x-001122334455" {
		t.Fatalf("reopened device id = %q", store.Snapshot().Companion.DeviceID)
	}
}

func TestOpenConfigReturnsMetadataFailure(t *testing.T) {
	t.Parallel()

	_, _, err := openConfig(filepath.Join(t.TempDir(), "config.yaml"), func() (config.Metadata, error) {
		return config.Metadata{}, errors.New("metadata unavailable")
	})
	if err == nil {
		t.Fatal("open config succeeded")
	}
}

func TestServeRunsAPIAndWebUI(t *testing.T) {
	t.Parallel()

	apiListener := testListener(t)
	webUIListener := testListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- serve(
			ctx,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
			apiListener,
			&http.Server{Handler: http.HandlerFunc(writeOK)},
			webUIListener,
			&http.Server{Handler: http.HandlerFunc(writeOK)},
		)
	}()

	for _, listener := range []net.Listener{apiListener, webUIListener} {
		response, err := http.Get("http://" + listener.Addr().String())
		if err != nil {
			t.Fatalf("request %s: %v", listener.Addr(), err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status %s = %d, want 200", listener.Addr(), response.StatusCode)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("servers did not shut down")
	}
}

func testListener(t *testing.T) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	return listener
}

func writeOK(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

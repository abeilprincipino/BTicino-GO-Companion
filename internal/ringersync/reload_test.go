package ringersync

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type reloadHarness struct {
	reloader  *FirmwareReloader
	path      string
	listening []bool
	started   []string
}

func newReloadHarness(t *testing.T, create bool, listening ...bool) *reloadHarness {
	t.Helper()

	h := &reloadHarness{path: filepath.Join(t.TempDir(), "conf.xml"), listening: listening}
	if create {
		if err := os.WriteFile(h.path, []byte("<conf/>"), 0o600); err != nil {
			t.Fatal(err)
		}

		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(h.path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	h.reloader = &FirmwareReloader{
		Path:    h.path,
		SSHWait: time.Millisecond,
		SSHListening: func() bool {
			if len(h.listening) == 0 {
				return false
			}

			value := h.listening[0]
			h.listening = h.listening[1:]

			return value
		},
		StartSSH: func(context.Context) error {
			h.started = append(h.started, "dropbear")
			return nil
		},
	}

	return h
}

func TestFirmwareReloadTouchesConfXML(t *testing.T) {
	h := newReloadHarness(t, true, true, true)

	if err := h.reloader.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(h.path)
	if err != nil {
		t.Fatal(err)
	}

	if time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("conf.xml mtime = %v, want now", info.ModTime())
	}

	if len(h.started) != 0 {
		t.Fatalf("started %v with ssh still listening", h.started)
	}
}

// c300x-controller creates an empty conf.xml when it is missing; bt_daemon
// reads that file, so the reload refuses instead.
func TestFirmwareReloadNeverCreatesConfXML(t *testing.T) {
	h := newReloadHarness(t, false, true)

	err := h.reloader.Reload(context.Background())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Reload() error = %v, want fs.ErrNotExist", err)
	}

	if _, statErr := os.Stat(h.path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("conf.xml exists after a refused reload: %v", statErr)
	}
}

// The reload stops dropbear too (seen on a C100X): it is started again when it
// was listening before.
func TestFirmwareReloadStartsSSHAgainWhenTheReloadStoppedIt(t *testing.T) {
	h := newReloadHarness(t, true, true, false)

	if err := h.reloader.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(h.started) != 1 {
		t.Fatalf("started = %v, want dropbear once", h.started)
	}
}

func TestFirmwareReloadLeavesSSHOffWhenItWasOff(t *testing.T) {
	h := newReloadHarness(t, true, false, false)

	if err := h.reloader.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(h.started) != 0 {
		t.Fatalf("started %v although ssh was off before the reload", h.started)
	}
}

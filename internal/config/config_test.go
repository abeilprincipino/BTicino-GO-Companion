package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	created, err := Create(path, Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"})
	if err != nil {
		t.Fatalf("create config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if loaded.Companion.DeviceID != created.Companion.DeviceID || loaded.Auth.ClaimCode != created.Auth.ClaimCode {
		t.Fatalf("loaded config differs: got %#v want %#v", loaded, created)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestCreateRejectsMissingMetadata(t *testing.T) {
	_, err := Create(filepath.Join(t.TempDir(), "config.yaml"), Metadata{})
	if !errors.Is(err, ErrMissingMetadata) {
		t.Fatalf("create error = %v, want ErrMissingMetadata", err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := Create(path, Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"}); err != nil {
		t.Fatalf("create config: %v", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open config: %v", err)
	}
	if _, err := file.WriteString("unknown: true\n"); err != nil {
		file.Close()
		t.Fatalf("append config: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close config: %v", err)
	}
	_, err = Load(path)
	if err == nil || !strings.Contains(err.Error(), "field unknown") {
		t.Fatalf("load error = %v, want unknown field", err)
	}
}

func TestStoreUpdateIsAtomicAndValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := Create(path, Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"}); err != nil {
		t.Fatalf("create config: %v", err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Update(func(cfg *Config) error {
		cfg.Companion.Name = "Front Door"
		return nil
	}); err != nil {
		t.Fatalf("update store: %v", err)
	}
	if got := store.Snapshot().Companion.Name; got != "Front Door" {
		t.Fatalf("name = %q, want Front Door", got)
	}
	if err := store.Update(func(cfg *Config) error {
		cfg.Companion.Entrypoints = []Entrypoint{}
		return nil
	}); err == nil {
		t.Fatal("invalid update succeeded")
	}
	if got := store.Snapshot().Companion.Name; got != "Front Door" {
		t.Fatalf("invalid update changed store: %q", got)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.yaml")
	want := State{LastEventID: "event-12"}
	if err := SaveState(path, want); err != nil {
		t.Fatalf("save state: %v", err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got != want {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
}

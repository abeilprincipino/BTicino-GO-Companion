package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testMAC   = "00:11:22:33:44:55"
	testModel = "C300X"
)

func TestCreateAndLoad(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	created, err := Create(path, Metadata{Model: testModel, MAC: testMAC})
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
	t.Parallel()

	_, err := Create(filepath.Join(t.TempDir(), "config.yaml"), Metadata{})
	if !errors.Is(err, ErrMissingMetadata) {
		t.Fatalf("create error = %v, want ErrMissingMetadata", err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := Create(path, Metadata{Model: testModel, MAC: testMAC}); err != nil {
		t.Fatalf("create config: %v", err)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open config: %v", err)
	}

	if _, err := file.WriteString("unknown: true\n"); err != nil {
		_ = file.Close()

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
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := Create(path, Metadata{Model: testModel, MAC: testMAC}); err != nil {
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

func TestHomeKitPINGeneratedAndValidated(t *testing.T) {
	t.Parallel()

	cfg, err := Default(Metadata{Model: testModel, MAC: testMAC})
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.HomeKit.PIN) != 10 || cfg.HomeKit.PIN[3] != '-' || cfg.HomeKit.PIN[6] != '-' {
		t.Fatalf("homekit pin = %q, want XXX-XX-XXX", cfg.HomeKit.PIN)
	}

	cfg.HomeKit.Enabled = true

	cfg.HomeKit.PIN = "invalid"
	if err := Validate(cfg); !errors.Is(err, ErrInvalidHomeKitPIN) {
		t.Fatalf("Validate() error = %v, want %v", err, ErrInvalidHomeKitPIN)
	}
}

func TestClaimCodeFormat(t *testing.T) {
	t.Parallel()

	code, err := GenerateClaimCode()
	if err != nil {
		t.Fatalf("generate claim code: %v", err)
	}
	if !ValidClaimCode(code) {
		t.Fatalf("generated invalid claim code %q", code)
	}

	for _, invalid := range []string{"01234567", "0123_4567", "0123-456", "0123-45678", "zzzz-zzzz"} {
		if ValidClaimCode(invalid) {
			t.Errorf("ValidClaimCode(%q) = true, want false", invalid)
		}
	}
}

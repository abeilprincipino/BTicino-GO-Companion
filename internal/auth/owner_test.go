package auth

import (
	"bticino-go-companion/internal/config"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestValidateOwner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		username, password string
		valid              bool
	}{
		{name: "valid", username: "owner", password: "correct-horse", valid: true},
		{name: "blank username", username: "  ", password: "correct-horse"},
		{name: "short password", username: "owner", password: "short"},
		{name: "default username", username: DefaultOwnerUsername, password: "correct-horse"},
		{name: "default password", username: "owner", password: DefaultOwnerPassword},
		{name: "72 byte password", username: "owner", password: strings.Repeat("a", 72), valid: true},
		{name: "73 byte password", username: "owner", password: strings.Repeat("a", 73)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateOwner(tt.username, tt.password)
			if tt.valid && err != nil {
				t.Fatalf("ValidateOwner() error = %v, want nil", err)
			}

			if !tt.valid && !errors.Is(err, ErrInvalidOwner) {
				t.Fatalf("ValidateOwner() error = %v, want ErrInvalidOwner", err)
			}
		})
	}
}

func TestStore_SetupOwner(t *testing.T) {
	t.Parallel()

	store, backend := newSetupRequiredStore(t)

	code, err := store.SetupOwner(" owner ", "correct-horse")
	if err != nil {
		t.Fatalf("SetupOwner() error = %v", err)
	}

	cfg := backend.Snapshot()
	if cfg.Auth.PairingState != config.PairingStateClaimable {
		t.Fatalf("pairing state = %q, want claimable", cfg.Auth.PairingState)
	}

	if cfg.WebUI.AdminUsername != "owner" {
		t.Fatalf("admin username = %q, want trimmed %q", cfg.WebUI.AdminUsername, "owner")
	}

	if bcrypt.CompareHashAndPassword([]byte(cfg.WebUI.AdminPasswordHash), []byte("correct-horse")) != nil {
		t.Fatal("admin password hash does not match the owner password")
	}

	if cfg.WebUI.SessionSecret == "" {
		t.Fatal("owner setup left the session secret empty")
	}

	want, err := store.InitialClaimCode()
	if err != nil || code != want || !config.ValidClaimCode(code) {
		t.Fatalf("claim code = %q (InitialClaimCode %q, %v)", code, want, err)
	}

	if _, err := store.SetupOwner("other", "correct-horse"); !errors.Is(err, ErrClaimNotAllowed) {
		t.Fatalf("second SetupOwner() error = %v, want ErrClaimNotAllowed", err)
	}
}

func TestStore_SetupOwnerRejectsInvalidOwnerWithoutChangingConfig(t *testing.T) {
	t.Parallel()

	store, backend := newSetupRequiredStore(t)
	before := backend.Snapshot()

	if _, err := store.SetupOwner("owner", "short"); !errors.Is(err, ErrInvalidOwner) {
		t.Fatalf("SetupOwner() error = %v, want ErrInvalidOwner", err)
	}

	after := backend.Snapshot()
	if after.Auth.PairingState != config.PairingStateSetupRequired || after.WebUI != before.WebUI {
		t.Fatalf("rejected owner changed the config: %#v", after)
	}
}

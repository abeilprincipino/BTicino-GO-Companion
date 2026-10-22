package auth

import (
	"bticino-go-companion/internal/config"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// DefaultOwnerUsername and DefaultOwnerPassword are the web UI's bootstrap
// credentials. They only open the owner setup, so an owner may use neither.
const (
	DefaultOwnerUsername = "companion"
	DefaultOwnerPassword = "companion"

	minOwnerPasswordLength = 8
)

var ErrInvalidOwner = errors.New("invalid owner credentials")

// ValidateOwner reports the first rule the owner credentials break. The
// username is compared trimmed, as it is stored.
func ValidateOwner(username, password string) error {
	username = strings.TrimSpace(username)

	switch {
	case username == "":
		return fmt.Errorf("%w: username is required", ErrInvalidOwner)
	case len(password) < minOwnerPasswordLength:
		return fmt.Errorf("%w: password must contain at least %d characters", ErrInvalidOwner, minOwnerPasswordLength)
	case username == DefaultOwnerUsername:
		return fmt.Errorf("%w: username must replace the default", ErrInvalidOwner)
	case password == DefaultOwnerPassword:
		return fmt.Errorf("%w: password must replace the default", ErrInvalidOwner)
	}

	return nil
}

// SetupOwner sets the web UI owner of a companion that has none and opens the
// initial claim, in a single config update, and returns the claim code. It is
// the owner setup the web UI does on its first login, available to callers
// that already proved they own the device.
func (s *Store) SetupOwner(username, password string) (string, error) {
	if s.config == nil {
		return "", ErrStoreUnavailable
	}

	username = strings.TrimSpace(username)
	if err := ValidateOwner(username, password); err != nil {
		return "", err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash owner password: %w", err)
	}

	secret, err := config.RandomHex(32)
	if err != nil {
		return "", fmt.Errorf("generate session secret: %w", err)
	}

	if err := s.config.Update(func(cfg *config.Config) error {
		if cfg.Auth.PairingState != config.PairingStateSetupRequired {
			return ErrClaimNotAllowed
		}

		cfg.WebUI.AdminUsername = username
		cfg.WebUI.AdminPasswordHash = string(hash)
		cfg.WebUI.SessionSecret = secret
		cfg.Auth.PairingState = config.PairingStateClaimable

		return nil
	}); err != nil {
		return "", err
	}

	return s.InitialClaimCode()
}

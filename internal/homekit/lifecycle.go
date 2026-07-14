package homekit

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"

	"bticino-go-companion/internal/config"
)

var ErrConfigStoreUnavailable = errors.New("homekit: config store is unavailable")

type ConfigStore interface {
	Snapshot() config.Config
	Update(func(*config.Config) error) error
}

type Lifecycle interface {
	Enable() (string, error)
	Disable() error
	Enabled() bool
}

type Manager struct {
	config ConfigStore
}

func NewManager(store ConfigStore) (*Manager, error) {
	if store == nil {
		return nil, ErrConfigStoreUnavailable
	}
	if err := config.Validate(store.Snapshot()); err != nil {
		return nil, err
	}
	return &Manager{config: store}, nil
}

func (m *Manager) Enable() (string, error) {
	if m == nil || m.config == nil {
		return "", ErrConfigStoreUnavailable
	}

	var pin string
	err := m.config.Update(func(cfg *config.Config) error {
		pin = cfg.HomeKit.PIN
		if pin == "" {
			var err error
			pin, err = newPIN()
			if err != nil {
				return err
			}
			cfg.HomeKit.PIN = pin
		}
		cfg.HomeKit.Enabled = true
		return nil
	})
	if err != nil {
		return "", err
	}
	return pin, nil
}

func (m *Manager) Disable() error {
	if m == nil || m.config == nil {
		return ErrConfigStoreUnavailable
	}
	return m.config.Update(func(cfg *config.Config) error {
		cfg.HomeKit.Enabled = false
		return nil
	})
}

func (m *Manager) Enabled() bool {
	return m != nil && m.config != nil && m.config.Snapshot().HomeKit.Enabled
}

func newPIN() (string, error) {
	for {
		value, err := rand.Int(rand.Reader, big.NewInt(100000000))
		if err != nil {
			return "", fmt.Errorf("generate homekit pin: %w", err)
		}
		pin := fmt.Sprintf("%03d-%02d-%03d", value.Int64()/100000, value.Int64()/1000%100, value.Int64()%1000)
		if pin != "111-11-111" && pin != "123-45-678" {
			return pin, nil
		}
	}
}

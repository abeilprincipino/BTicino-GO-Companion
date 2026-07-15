package homekit

import (
	"bticino-go-companion/internal/config"
	"errors"
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

			pin, err = config.GenerateHomeKitPIN()
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

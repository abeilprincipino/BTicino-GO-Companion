package homekit

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"
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
	logger *slog.Logger

	mu        sync.Mutex
	unlocker  Unlocker
	audio     AudioController
	voicemail VoicemailController
	state     core.State

	bridge    *accessory.Bridge
	accessory []*accessory.A
	locks     map[core.EntrypointID]*lockAccessory
	ringer    *accessory.Switch
	mailbox   *accessory.Switch
	doorbell  *doorbellAccessory
}

type Unlocker interface {
	Unlock(context.Context, core.EntrypointID) error
}

type AudioController interface {
	Mute(context.Context) error
	Unmute(context.Context) error
}

type VoicemailController interface {
	Enable(context.Context) error
	Disable(context.Context) error
}

type lockAccessory struct {
	lock *service.LockMechanism
	id   core.EntrypointID
	aid  uint64
}

type doorbellAccessory struct {
	service *service.Doorbell
}

type runtimeConfig struct {
	name         string
	address      string
	setupPIN     string
	manufacturer string
	model        string
	serialNumber string
}

func NewManager(store ConfigStore) (*Manager, error) {
	if store == nil {
		return nil, ErrConfigStoreUnavailable
	}

	if err := config.Validate(store.Snapshot()); err != nil {
		return nil, err
	}

	return &Manager{config: store, logger: slog.Default(), locks: make(map[core.EntrypointID]*lockAccessory)}, nil
}

// SetControllers supplies the existing Companion controls used by HomeKit
// characteristic writes. It must be called before Run.
func (m *Manager) SetControllers(unlocker Unlocker, audio AudioController, voicemail VoicemailController) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.unlocker = unlocker
	m.audio = audio
	m.voicemail = voicemail
}

// Sync projects Companion state into already published HomeKit accessories.
func (m *Manager) Sync(state core.State) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	previousRing := m.state.PhysicalRing
	m.state = state
	m.syncLocked(previousRing == nil && state.PhysicalRing != nil)
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

// Run serves the persistent HomeKit bridge until ctx is canceled. It returns
// immediately without starting a listener when HomeKit is disabled.
func (m *Manager) Run(ctx context.Context, dataDir string, logger *slog.Logger) error {
	if m == nil || m.config == nil {
		return ErrConfigStoreUnavailable
	}
	if ctx == nil {
		return errors.New("homekit: context is required")
	}
	if strings.TrimSpace(dataDir) == "" {
		return errors.New("homekit: data directory is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	m.mu.Lock()
	m.logger = logger.With("component", "homekit")
	m.mu.Unlock()

	cfg := m.config.Snapshot()
	if !cfg.HomeKit.Enabled {
		return nil
	}
	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("validate homekit configuration: %w", err)
	}

	storePath := filepath.Join(dataDir, "homekit")
	if err := os.MkdirAll(storePath, 0o700); err != nil {
		return fmt.Errorf("create homekit store: %w", err)
	}
	if err := os.Chmod(storePath, 0o700); err != nil {
		return fmt.Errorf("secure homekit store: %w", err)
	}

	runtime := newRuntimeConfig(cfg)
	m.mu.Lock()
	bridge, accessories := m.buildAccessoriesLocked(cfg, runtime)
	m.syncLocked(false)
	m.mu.Unlock()

	server, err := hap.NewServer(hap.NewFsStore(storePath), bridge.A, accessories...)
	if err != nil {
		return fmt.Errorf("create homekit server: %w", err)
	}
	server.Pin = runtime.setupPIN
	server.Addr = runtime.address

	logger.Info("homekit bridge starting", "name", runtime.name, "address", server.Addr, "store", storePath)
	if err := server.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("serve homekit bridge: %w", err)
	}
	return nil
}

func (m *Manager) buildAccessoriesLocked(cfg config.Config, runtime runtimeConfig) (*accessory.Bridge, []*accessory.A) {
	bridge := accessory.NewBridge(accessory.Info{
		Name:         runtime.name,
		Manufacturer: runtime.manufacturer,
		Model:        runtime.model,
		SerialNumber: runtime.serialNumber,
	})
	bridge.Id = 1

	m.bridge = bridge
	m.accessory = nil
	m.locks = make(map[core.EntrypointID]*lockAccessory)
	m.ringer = nil
	m.mailbox = nil
	m.doorbell = nil

	if m.audio != nil {
		ringer := accessory.NewSwitch(accessory.Info{
			Name:         "Ringer Mute",
			Manufacturer: runtime.manufacturer,
			Model:        runtime.model,
			SerialNumber: runtime.serialNumber + "-ringer",
		})
		ringer.Id = 2
		ringer.Switch.On.OnValueRemoteUpdate(m.setRingerMute)
		m.ringer = ringer
		m.accessory = append(m.accessory, ringer.A)
	}

	if m.voicemail != nil && m.state.Voicemail != nil {
		mailbox := accessory.NewSwitch(accessory.Info{
			Name:         "Voicemail",
			Manufacturer: runtime.manufacturer,
			Model:        runtime.model,
			SerialNumber: runtime.serialNumber + "-voicemail",
		})
		mailbox.Id = 3
		mailbox.Switch.On.OnValueRemoteUpdate(m.setVoicemail)
		m.mailbox = mailbox
		m.accessory = append(m.accessory, mailbox.A)
	}

	doorbell := accessory.New(accessory.Info{
		Name:         "Doorbell",
		Manufacturer: runtime.manufacturer,
		Model:        runtime.model,
		SerialNumber: runtime.serialNumber + "-doorbell",
	}, accessory.TypeVideoDoorbell)
	doorbell.Id = 4
	doorbellService := service.NewDoorbell()
	doorbell.AddS(doorbellService.S)
	m.doorbell = &doorbellAccessory{service: doorbellService}
	m.accessory = append(m.accessory, doorbell)

	entrypoints := append([]config.Entrypoint(nil), cfg.Companion.Entrypoints...)
	sort.Slice(entrypoints, func(i, j int) bool { return entrypoints[i].ID < entrypoints[j].ID })
	usedIDs := map[uint64]struct{}{1: {}, 2: {}, 3: {}, 4: {}}
	for _, entrypoint := range entrypoints {
		if !entrypoint.Capabilities.Unlock || m.unlocker == nil {
			continue
		}

		lock := accessory.New(accessory.Info{
			Name:         entrypoint.Label,
			Manufacturer: runtime.manufacturer,
			Model:        runtime.model,
			SerialNumber: runtime.serialNumber + "-lock-" + entrypoint.ID,
		}, accessory.TypeDoorLock)
		lock.Id = stableEntrypointAccessoryID(entrypoint.ID, usedIDs)
		mechanism := service.NewLockMechanism()
		mechanism.LockTargetState.OnValueRemoteUpdate(func(target int) {
			m.unlock(core.EntrypointID(entrypoint.ID), target)
		})
		lock.AddS(mechanism.S)
		m.locks[core.EntrypointID(entrypoint.ID)] = &lockAccessory{lock: mechanism, id: core.EntrypointID(entrypoint.ID), aid: lock.Id}
		m.accessory = append(m.accessory, lock)
	}

	return bridge, m.accessory
}

func stableEntrypointAccessoryID(entrypointID string, used map[uint64]struct{}) uint64 {
	hash := uint64(2166136261)
	for i := 0; i < len(entrypointID); i++ {
		hash ^= uint64(entrypointID[i])
		hash *= 16777619
	}

	id := 100 + (hash & 0x7fffffff)
	for {
		if _, exists := used[id]; !exists {
			used[id] = struct{}{}
			return id
		}
		id++
	}
}

func (m *Manager) syncLocked(ringStarted bool) {
	if m.ringer != nil {
		m.ringer.Switch.On.SetValue(m.state.Audio.Muted)
	}
	if m.mailbox != nil && m.state.Voicemail != nil {
		m.mailbox.Switch.On.SetValue(m.state.Voicemail.Enabled)
	}
	for _, lock := range m.locks {
		m.setLockState(lock, characteristic.LockTargetStateSecured)
	}
	if ringStarted && m.doorbell != nil {
		if err := m.doorbell.service.ProgrammableSwitchEvent.SetValue(characteristic.ProgrammableSwitchEventSinglePress); err != nil {
			m.logger.Error("set homekit doorbell event", "error", err)
		}
	}
}

func (m *Manager) unlock(entrypointID core.EntrypointID, target int) {
	if target != characteristic.LockTargetStateUnsecured {
		m.restoreState()
		return
	}

	m.mu.Lock()
	unlocker := m.unlocker
	m.mu.Unlock()
	if unlocker == nil {
		m.restoreState()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := unlocker.Unlock(ctx, entrypointID)
	cancel()
	if err != nil {
		m.logger.Error("homekit unlock failed", "entrypoint_id", entrypointID, "error", err)
	}
	m.restoreState()
}

func (m *Manager) setRingerMute(muted bool) {
	m.mu.Lock()
	audio := m.audio
	m.mu.Unlock()
	if audio == nil {
		m.restoreState()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var err error
	if muted {
		err = audio.Mute(ctx)
	} else {
		err = audio.Unmute(ctx)
	}
	cancel()
	if err != nil {
		m.logger.Error("homekit ringer mute update failed", "muted", muted, "error", err)
	}
	m.restoreState()
}

func (m *Manager) setVoicemail(enabled bool) {
	m.mu.Lock()
	voicemail := m.voicemail
	m.mu.Unlock()
	if voicemail == nil {
		m.restoreState()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var err error
	if enabled {
		err = voicemail.Enable(ctx)
	} else {
		err = voicemail.Disable(ctx)
	}
	cancel()
	if err != nil {
		m.logger.Error("homekit voicemail update failed", "enabled", enabled, "error", err)
	}
	m.restoreState()
}

func (m *Manager) restoreState() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncLocked(false)
}

func (m *Manager) setLockState(lock *lockAccessory, target int) {
	if err := lock.lock.LockCurrentState.SetValue(target); err != nil {
		m.logger.Error("set homekit lock current state", "entrypoint_id", lock.id, "error", err)
	}
	if err := lock.lock.LockTargetState.SetValue(target); err != nil {
		m.logger.Error("set homekit lock target state", "entrypoint_id", lock.id, "error", err)
	}
}

func newRuntimeConfig(cfg config.Config) runtimeConfig {
	name := strings.TrimSpace(cfg.HomeKit.Name)
	if name == "" {
		name = "BTicino Companion"
	}
	port := cfg.HomeKit.Port
	if port == 0 {
		port = 51826
	}

	return runtimeConfig{
		name:         name,
		address:      fmt.Sprintf(":%d", port),
		setupPIN:     strings.ReplaceAll(cfg.HomeKit.PIN, "-", ""),
		manufacturer: "BTicino",
		model:        cfg.Companion.Model,
		serialNumber: cfg.Companion.DeviceID,
	}
}

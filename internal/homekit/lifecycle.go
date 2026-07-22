package homekit

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

	mu           sync.Mutex
	unlocker     Unlocker
	audio        AudioController
	voicemail    VoicemailController
	state        core.State
	unlockTimers map[core.EntrypointID]*time.Timer

	bridge    *accessory.Bridge
	accessory []*accessory.A
	locks     map[core.EntrypointID]*lockAccessory
	ringer    *accessory.Switch
	mailbox   *accessory.Switch
	doorbells map[core.EntrypointID]*videoDoorbellAccessory
	stream    *media.StreamCoordinator
	snapshots SnapshotProvider
	server    *hap.Server
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

// SnapshotProvider returns the last captured JPEG without starting media.
type SnapshotProvider interface {
	Latest(entrypointID string) ([]byte, error)
}

// Status reports the bridge lifecycle and persistent HomeKit pairing state.
type Status struct {
	Enabled bool `json:"enabled"`
	Running bool `json:"running"`
	Paired  bool `json:"paired"`
}

type lockAccessory struct {
	lock *service.LockMechanism
	id   core.EntrypointID
	aid  uint64
}

type videoDoorbellAccessory struct {
	stream   *cameraSessionManager
	doorbell *service.Doorbell
	id       core.EntrypointID
	aid      uint64
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

	return &Manager{config: store, logger: slog.Default(), locks: make(map[core.EntrypointID]*lockAccessory), unlockTimers: make(map[core.EntrypointID]*time.Timer), doorbells: make(map[core.EntrypointID]*videoDoorbellAccessory)}, nil
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

// SetStreamCoordinator supplies the sole owner of the intercom media source.
// It must be called before Run to publish stream-capable camera accessories.
func (m *Manager) SetStreamCoordinator(coordinator *media.StreamCoordinator) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.stream = coordinator
}

// SetSnapshotProvider supplies cached still images for HomeKit /resource requests.
// It must be called before Run.
func (m *Manager) SetSnapshotProvider(provider SnapshotProvider) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.snapshots = provider
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
	if state.PhysicalRing != nil && (previousRing == nil || previousRing.EntrypointID != state.PhysicalRing.EntrypointID) {
		m.ringDoorbellLocked(state.PhysicalRing.EntrypointID)
	}

	m.syncLocked()
}

func (m *Manager) Enable() (string, error) {
	if m == nil || m.config == nil {
		return "", ErrConfigStoreUnavailable
	}

	err := m.config.Update(func(cfg *config.Config) error {
		cfg.HomeKit.Enabled = true
		return nil
	})
	if err != nil {
		return "", err
	}

	return "", nil
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

func (m *Manager) Status() Status {
	if m == nil || m.config == nil {
		return Status{}
	}

	status := Status{Enabled: m.config.Snapshot().HomeKit.Enabled}
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.server != nil {
		status.Running = true
		status.Paired = m.server.IsPaired()
	}

	return status
}

// Reset clears the HAP identity and pairing store. The next Companion restart
// creates new setup credentials before the bridge starts.
func (m *Manager) Reset(dataDir string) error {
	if m == nil || m.config == nil {
		return ErrConfigStoreUnavailable
	}

	if strings.TrimSpace(dataDir) == "" {
		return errors.New("homekit: data directory is required")
	}

	if err := m.config.Update(func(cfg *config.Config) error {
		cfg.HomeKit.Enabled = true
		cfg.HomeKit.PIN = ""
		cfg.HomeKit.SetupID = ""

		return nil
	}); err != nil {
		return fmt.Errorf("clear HomeKit setup credentials: %w", err)
	}

	if err := os.RemoveAll(filepath.Join(dataDir, "homekit")); err != nil {
		return fmt.Errorf("remove HomeKit store: %w", err)
	}

	return nil
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

	if cfg.HomeKit.PIN == "" || cfg.HomeKit.SetupID == "" {
		if err := m.config.Update(func(cfg *config.Config) error {
			if cfg.HomeKit.PIN == "" {
				pin, err := config.GenerateHomeKitPIN()
				if err != nil {
					return err
				}

				cfg.HomeKit.PIN = pin
			}

			if cfg.HomeKit.SetupID == "" {
				setupID, err := config.GenerateHomeKitSetupID()
				if err != nil {
					return err
				}

				cfg.HomeKit.SetupID = setupID
			}

			return nil
		}); err != nil {
			return fmt.Errorf("generate HomeKit setup credentials: %w", err)
		}

		cfg = m.config.Snapshot()
	}

	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("validate homekit configuration: %w", err)
	}

	storePath := filepath.Join(dataDir, "homekit")
	if err := os.MkdirAll(storePath, 0o700); err != nil {
		return fmt.Errorf("create homekit store: %w", err)
	}

	if err := os.Chmod(storePath, 0o700); err != nil { // #nosec G302 -- directory requires execute permission for HomeKit's private store.
		return fmt.Errorf("secure homekit store: %w", err)
	}

	runtime := newRuntimeConfig(cfg)

	m.mu.Lock()
	bridge, accessories := m.buildAccessoriesLocked(cfg, runtime)
	m.syncLocked()
	m.mu.Unlock()

	server, err := hap.NewServer(hap.NewFsStore(storePath), bridge.A, accessories...)
	if err != nil {
		return fmt.Errorf("create homekit server: %w", err)
	}

	server.Pin = runtime.setupPIN
	server.SetupId = cfg.HomeKit.SetupID
	server.Addr = runtime.address
	server.ServeMux().HandleFunc("/resource", m.resourceHandler(server.IsAuthorized))
	m.mu.Lock()
	m.server = server

	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.server == server {
			m.server = nil
		}
		m.mu.Unlock()
	}()

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
	for _, timer := range m.unlockTimers {
		timer.Stop()
	}

	m.unlockTimers = make(map[core.EntrypointID]*time.Timer)
	m.locks = make(map[core.EntrypointID]*lockAccessory)
	m.ringer = nil
	m.mailbox = nil
	m.doorbells = make(map[core.EntrypointID]*videoDoorbellAccessory)

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

	entrypoints := append([]config.Entrypoint(nil), cfg.Companion.Entrypoints...)
	sort.Slice(entrypoints, func(i, j int) bool { return entrypoints[i].ID < entrypoints[j].ID })

	usedIDs := map[uint64]struct{}{1: {}, 2: {}, 3: {}}

	for _, entrypoint := range entrypoints {
		entrypointID := core.EntrypointID(entrypoint.ID)
		if entrypoint.Capabilities.Stream && m.stream != nil {
			doorbell := accessory.New(accessory.Info{
				Name:         entrypoint.Label,
				Manufacturer: runtime.manufacturer,
				Model:        runtime.model,
				SerialNumber: runtime.serialNumber + "-doorbell-" + entrypoint.ID,
			}, accessory.TypeVideoDoorbell)
			doorbell.Id = stableEntrypointAccessoryID("doorbell-"+entrypoint.ID, usedIDs)
			doorbellService := service.NewDoorbell()
			doorbellService.Primary = true
			cameraControl := service.NewCameraControl()
			cameraControl.On.SetValue(true)

			cameraService := service.NewCameraRTPStreamManagement()

			cameraActive := characteristic.NewActive()
			if err := cameraActive.SetValue(characteristic.ActiveActive); err != nil {
				m.logger.Warn("set HomeKit camera active state", "entrypoint_id", entrypoint.ID, "error", err)
			}

			cameraService.AddC(cameraActive.C)
			doorbell.AddS(doorbellService.S)
			doorbell.AddS(cameraControl.S)
			doorbell.AddS(cameraService.S)
			m.doorbells[entrypointID] = &videoDoorbellAccessory{
				stream:   newCameraSessionManager(m.stream, entrypoint, cameraService, m.logger),
				doorbell: doorbellService,
				id:       entrypointID,
				aid:      doorbell.Id,
			}
			m.accessory = append(m.accessory, doorbell)
		}

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
		mechanism.LockTargetState.OnSetRemoteValue(func(target int) error {
			if target != characteristic.LockTargetStateUnsecured {
				return nil
			}

			return m.unlock(entrypointID)
		})
		lock.AddS(mechanism.S)
		lockAccessory := &lockAccessory{lock: mechanism, id: entrypointID, aid: lock.Id}
		m.locks[entrypointID] = lockAccessory
		m.setLockState(lockAccessory, characteristic.LockTargetStateSecured)
		m.accessory = append(m.accessory, lock)
	}

	return bridge, m.accessory
}

func (m *Manager) ringDoorbellLocked(entrypointID core.EntrypointID) {
	doorbell := m.doorbells[entrypointID]
	if doorbell == nil || doorbell.doorbell == nil {
		return
	}

	if err := doorbell.doorbell.ProgrammableSwitchEvent.SetValue(characteristic.ProgrammableSwitchEventSinglePress); err != nil {
		m.logger.Warn("set HomeKit doorbell event", "entrypoint_id", entrypointID, "error", err)
		return
	}

	m.logger.Info("homekit doorbell pressed", "entrypoint_id", entrypointID)
}

type resourceRequest struct {
	AccessoryID  uint64 `json:"aid"`
	ResourceType string `json:"resource-type"`
}

func (m *Manager) resourceHandler(authorized func(*http.Request) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if authorized == nil || !authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var request resourceRequest

		decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
		if err := decoder.Decode(&request); err != nil || request.AccessoryID == 0 || request.ResourceType != "image" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		m.mu.Lock()

		var entrypointID string

		for _, doorbell := range m.doorbells {
			if doorbell.aid == request.AccessoryID {
				entrypointID = string(doorbell.id)
				break
			}
		}

		provider := m.snapshots
		m.mu.Unlock()

		if entrypointID == "" || provider == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		image, err := provider.Latest(entrypointID)
		if err != nil {
			if errors.Is(err, media.ErrSnapshotNotFound) || errors.Is(err, media.ErrSnapshotUnavailable) {
				w.WriteHeader(http.StatusNotFound)
				return
			}

			m.logger.Warn("read HomeKit snapshot", "entrypoint_id", entrypointID, "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(image)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(image)
	}
}

func stableEntrypointAccessoryID(entrypointID string, used map[uint64]struct{}) uint64 {
	hash := uint64(2166136261)
	for i := range len(entrypointID) {
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

func (m *Manager) syncLocked() {
	if m.ringer != nil {
		m.ringer.Switch.On.SetValue(m.state.Audio.Muted)
	}

	if m.mailbox != nil && m.state.Voicemail != nil {
		m.mailbox.Switch.On.SetValue(m.state.Voicemail.Enabled)
	}
}

func (m *Manager) unlock(entrypointID core.EntrypointID) error {
	m.mu.Lock()
	unlocker := m.unlocker
	m.mu.Unlock()

	if unlocker == nil {
		return errors.New("homekit unlock control is unavailable")
	}

	m.logger.Info("homekit unlock requested", "entrypoint_id", entrypointID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := unlocker.Unlock(ctx, entrypointID)

	cancel()

	if err != nil {
		m.logger.Error("homekit unlock failed", "entrypoint_id", entrypointID, "error", err)
		return err
	}

	m.markUnlocking(entrypointID)
	m.logger.Info("homekit unlock completed", "entrypoint_id", entrypointID)

	return nil
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

	m.syncLocked()
}

func (m *Manager) markUnlocking(entrypointID core.EntrypointID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	lock := m.locks[entrypointID]
	if lock == nil {
		return
	}

	if timer := m.unlockTimers[entrypointID]; timer != nil {
		timer.Stop()
	}

	m.setLockState(lock, characteristic.LockCurrentStateUnsecured)

	var timer *time.Timer

	timer = time.AfterFunc(1500*time.Millisecond, func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		if m.unlockTimers[entrypointID] != timer {
			return
		}

		delete(m.unlockTimers, entrypointID)
		m.setLockState(lock, characteristic.LockTargetStateSecured)
	})
	m.unlockTimers[entrypointID] = timer
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

	return runtimeConfig{
		name:         name,
		address:      ":51826",
		setupPIN:     strings.ReplaceAll(cfg.HomeKit.PIN, "-", ""),
		manufacturer: "BTicino",
		model:        cfg.Companion.Model,
		serialNumber: cfg.Companion.DeviceID,
	}
}

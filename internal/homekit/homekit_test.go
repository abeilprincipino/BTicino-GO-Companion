package homekit

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/brutella/hap/characteristic"
)

func TestManager_EnableDisablePreservesPIN(t *testing.T) {
	t.Parallel()

	store := testConfigStore(t)

	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}

	pin, err := manager.Enable()
	if err != nil {
		t.Fatal(err)
	}

	if !manager.Enabled() {
		t.Fatal("Enabled() = false, want true")
	}

	if err := manager.Disable(); err != nil {
		t.Fatal(err)
	}

	if manager.Enabled() {
		t.Fatal("Enabled() = true, want false")
	}

	if got := store.Snapshot().HomeKit.PIN; got != pin {
		t.Fatalf("pin = %q, want %q", got, pin)
	}
}

func TestManager_EnableGeneratesPersistentPIN(t *testing.T) {
	t.Parallel()

	store := testConfigStore(t)
	if err := store.Update(func(cfg *config.Config) error {
		cfg.HomeKit.PIN = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}

	pin, err := manager.Enable()
	if err != nil {
		t.Fatal(err)
	}

	if len(pin) != 10 || pin[3] != '-' || pin[6] != '-' {
		t.Fatalf("pin = %q, want XXX-XX-XXX", pin)
	}

	if got := store.Snapshot().HomeKit.PIN; got != pin {
		t.Fatalf("stored pin = %q, want %q", got, pin)
	}
}

func TestManager_RejectsUnavailableStore(t *testing.T) {
	t.Parallel()

	if _, err := NewManager(nil); !errors.Is(err, ErrConfigStoreUnavailable) {
		t.Fatalf("NewManager() error = %v, want %v", err, ErrConfigStoreUnavailable)
	}
}

func TestNewRuntimeConfig(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Companion: config.Companion{Model: "C300X", DeviceID: "c300x-001122334455"},
		HomeKit:   config.HomeKit{PIN: "123-45-678"},
	}

	runtime := newRuntimeConfig(cfg)
	if runtime.name != "BTicino Companion" {
		t.Fatalf("name = %q, want default", runtime.name)
	}
	if runtime.address != ":51826" {
		t.Fatalf("address = %q, want :51826", runtime.address)
	}
	if runtime.setupPIN != "12345678" {
		t.Fatalf("setup pin = %q, want 12345678", runtime.setupPIN)
	}
	if runtime.model != "C300X" || runtime.serialNumber != "c300x-001122334455" {
		t.Fatalf("accessory identity = %#v", runtime)
	}
}

func TestNewRuntimeConfigUsesConfiguredValues(t *testing.T) {
	t.Parallel()

	runtime := newRuntimeConfig(config.Config{HomeKit: config.HomeKit{
		Name: "Front Door",
		Port: 12345,
		PIN:  "123-45-678",
	}})
	if runtime.name != "Front Door" || runtime.address != ":12345" {
		t.Fatalf("runtime config = %#v", runtime)
	}
}

func TestManager_RunSkipsDisabledHomeKit(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(testConfigStore(t))
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "homekit-data")
	if err := manager.Run(context.Background(), dataDir, nil); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled Run() created %q: %v", dataDir, err)
	}
}

func TestManager_BuildsStableControlAccessories(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(testConfigStore(t))
	if err != nil {
		t.Fatal(err)
	}
	controls := &homeKitControls{}
	manager.SetControllers(controls, controls, controls)
	manager.Sync(core.State{Audio: core.AudioState{Muted: true}, Voicemail: &core.VoicemailState{Enabled: true}})

	cfg := testConfigStore(t).Snapshot()
	cfg.Companion.Entrypoints = []config.Entrypoint{
		{ID: "side", Label: "Side Gate", Capabilities: config.Capabilities{Unlock: true}},
		{ID: "main", Label: "Main Gate", Capabilities: config.Capabilities{Unlock: true}},
	}

	manager.mu.Lock()
	bridge, accessories := manager.buildAccessoriesLocked(cfg, newRuntimeConfig(cfg))
	manager.syncLocked(false)
	manager.mu.Unlock()

	if bridge.Id != 1 {
		t.Fatalf("bridge id = %d, want 1", bridge.Id)
	}
	if len(accessories) != 5 {
		t.Fatalf("accessory count = %d, want 5", len(accessories))
	}
	if manager.ringer == nil || !manager.ringer.Switch.On.Value() {
		t.Fatal("ringer mute switch did not reflect projected state")
	}
	if manager.mailbox == nil || !manager.mailbox.Switch.On.Value() {
		t.Fatal("voicemail switch did not reflect projected state")
	}
	if manager.doorbell == nil {
		t.Fatal("doorbell accessory was not created")
	}
	for _, id := range []core.EntrypointID{"main", "side"} {
		lock := manager.locks[id]
		if lock == nil {
			t.Fatalf("lock %q was not created", id)
		}
		if got := lock.lock.LockTargetState.Value(); got != characteristic.LockTargetStateSecured {
			t.Fatalf("lock %q target state = %d, want secured", id, got)
		}
	}
	mainID := manager.locks["main"].aid
	manager.mu.Lock()
	cfg.Companion.Entrypoints[0], cfg.Companion.Entrypoints[1] = cfg.Companion.Entrypoints[1], cfg.Companion.Entrypoints[0]
	manager.buildAccessoriesLocked(cfg, newRuntimeConfig(cfg))
	manager.mu.Unlock()
	if got := manager.locks["main"].aid; got != mainID {
		t.Fatalf("main lock id = %d, want stable id %d", got, mainID)
	}
}

func TestManager_UnlockRestoresSecuredState(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(testConfigStore(t))
	if err != nil {
		t.Fatal(err)
	}
	controls := &homeKitControls{}
	manager.SetControllers(controls, nil, nil)
	cfg := testConfigStore(t).Snapshot()

	manager.mu.Lock()
	manager.buildAccessoriesLocked(cfg, newRuntimeConfig(cfg))
	manager.syncLocked(false)
	manager.mu.Unlock()

	manager.unlock("main", characteristic.LockTargetStateUnsecured)
	if controls.unlocked != "main" {
		t.Fatalf("unlock entrypoint = %q, want main", controls.unlocked)
	}
	if got := manager.locks["main"].lock.LockTargetState.Value(); got != characteristic.LockTargetStateSecured {
		t.Fatalf("lock target state = %d, want secured", got)
	}
}

func TestManager_ControlsRestoreProjectedState(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(testConfigStore(t))
	if err != nil {
		t.Fatal(err)
	}
	controls := &homeKitControls{}
	manager.SetControllers(nil, controls, controls)
	manager.Sync(core.State{Audio: core.AudioState{Muted: false}, Voicemail: &core.VoicemailState{Enabled: true}})
	cfg := testConfigStore(t).Snapshot()

	manager.mu.Lock()
	manager.buildAccessoriesLocked(cfg, newRuntimeConfig(cfg))
	manager.syncLocked(false)
	manager.mu.Unlock()

	manager.setRingerMute(true)
	manager.setVoicemail(false)
	if controls.muteCalls != 1 || controls.disableVoicemailCalls != 1 {
		t.Fatalf("control calls = %#v", controls)
	}
	if manager.ringer.Switch.On.Value() {
		t.Fatal("ringer switch optimistically changed projected state")
	}
	if !manager.mailbox.Switch.On.Value() {
		t.Fatal("voicemail switch optimistically changed projected state")
	}
}

func TestPersistentPairingStore(t *testing.T) {
	t.Parallel()

	state := NewFileStateStore(filepath.Join(t.TempDir(), "homekit.yaml"))

	store, err := NewPairingStore(state)
	if err != nil {
		t.Fatal(err)
	}

	pairing := Pairing{Identifier: "controller", PublicKey: make([]byte, 32), Admin: true}
	if err := store.Put(pairing); err != nil {
		t.Fatal(err)
	}

	got, ok, err := store.Get("controller")
	if err != nil || !ok {
		t.Fatalf("Get() = %#v, %t, %v", got, ok, err)
	}

	got.PublicKey[0] = 1

	again, ok, err := store.Get("controller")
	if err != nil || !ok || again.PublicKey[0] != 0 {
		t.Fatalf("stored pairing was mutated: %#v, %t, %v", again, ok, err)
	}

	if err := store.Delete("controller"); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete("controller"); !errors.Is(err, ErrPairingNotFound) {
		t.Fatalf("Delete() error = %v, want %v", err, ErrPairingNotFound)
	}
}

func TestPersistentPairingStore_RejectsInvalidPairing(t *testing.T) {
	t.Parallel()

	store, err := NewPairingStore(NewFileStateStore(filepath.Join(t.TempDir(), "homekit.yaml")))
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Put(Pairing{Identifier: "controller"}); !errors.Is(err, ErrInvalidPairing) {
		t.Fatalf("Put() error = %v, want %v", err, ErrInvalidPairing)
	}
}

func TestFileStateStoreUsesPrivatePermissions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "homekit.yaml")

	store := NewFileStateStore(path)
	if err := store.Save(State{}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

func TestMediaContracts(t *testing.T) {
	t.Parallel()

	var (
		_ media.Consumer   = media.ConsumerFunc(func(media.Packet) {})
		_ MediaDistributor = media.NewDistributor()
	)
}

type homeKitControls struct {
	unlocked              core.EntrypointID
	muteCalls             int
	unmuteCalls           int
	enableVoicemailCalls  int
	disableVoicemailCalls int
}

func (c *homeKitControls) Unlock(_ context.Context, id core.EntrypointID) error {
	c.unlocked = id
	return nil
}

func (c *homeKitControls) Mute(context.Context) error {
	c.muteCalls++
	return nil
}

func (c *homeKitControls) Unmute(context.Context) error {
	c.unmuteCalls++
	return nil
}

func (c *homeKitControls) Enable(context.Context) error {
	c.enableVoicemailCalls++
	return nil
}

func (c *homeKitControls) Disable(context.Context) error {
	c.disableVoicemailCalls++
	return nil
}

func testConfigStore(t *testing.T) *config.Store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.Create(path, config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"}); err != nil {
		t.Fatal(err)
	}

	store, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	return store
}

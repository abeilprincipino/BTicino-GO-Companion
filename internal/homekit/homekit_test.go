package homekit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/media"
)

func TestManager_EnableDisablePreservesPIN(t *testing.T) {
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
	if _, err := NewManager(nil); !errors.Is(err, ErrConfigStoreUnavailable) {
		t.Fatalf("NewManager() error = %v, want %v", err, ErrConfigStoreUnavailable)
	}
}

func TestPersistentPairingStore(t *testing.T) {
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
	store, err := NewPairingStore(NewFileStateStore(filepath.Join(t.TempDir(), "homekit.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(Pairing{Identifier: "controller"}); !errors.Is(err, ErrInvalidPairing) {
		t.Fatalf("Put() error = %v, want %v", err, ErrInvalidPairing)
	}
}

func TestFileStateStoreUsesPrivatePermissions(t *testing.T) {
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
	var _ MediaConsumer = media.ConsumerFunc(func(media.Packet) {})
	var _ MediaDistributor = media.NewDistributor()
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

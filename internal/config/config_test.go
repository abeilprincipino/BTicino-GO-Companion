package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

	if loaded.Auth.PairingState != PairingStateSetupRequired || created.Auth.PairingState != PairingStateSetupRequired {
		t.Fatalf("loaded config differs: got %#v want %#v", loaded, created)
	}
	if loaded.Companion.DeviceID != "" || loaded.Companion.Model != "" {
		t.Fatalf("loaded runtime metadata = %#v, want empty", loaded.Companion)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestCreateWritesCompleteConfigYAML(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := Create(path, Metadata{Model: testModel, MAC: testMAC}); err != nil {
		t.Fatalf("create config: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode yaml: %v", err)
	}

	for _, path := range []string{
		"logging.level",
		"companion.entrypoints",
		"companion.entrypoints.0.id",
		"companion.entrypoints.0.label",
		"companion.entrypoints.0.devaddr",
		"companion.entrypoints.0.capabilities.stream",
		"companion.entrypoints.0.capabilities.unlock",
		"companion.entrypoints.0.capabilities.ring",
		"auth.home_assistant.pairing_state",
		"auth.home_assistant.instance_id",
		"auth.home_assistant.bearer_token_hash",
		"auth.webui.admin_username",
		"auth.webui.admin_password_hash",
		"auth.webui.session_secret",
		"system.reboot.enabled",
		"system.updates.enabled",
		"system.updates.exposed",
		"system.services.companion.enabled",
		"system.services.companion.exposed",
		"system.services.dropbear.enabled",
		"system.services.dropbear.exposed",
		"homekit.enabled",
		"homekit.pin",
		"homekit.setup_id",
		"homekit.name",
	} {
		if _, ok := yamlPath(document, path); !ok {
			t.Errorf("missing YAML key path %q", path)
		}
	}

	for _, path := range []string{"auth.home_assistant.bearer_token_hash", "auth.webui.admin_username", "auth.webui.admin_password_hash", "auth.webui.session_secret"} {
		if value, _ := yamlPath(document, path); value != "" {
			t.Errorf("YAML value at %q = %#v, want empty string", path, value)
		}
	}
	if value, _ := yamlPath(document, "auth.home_assistant.pairing_state"); value != string(PairingStateSetupRequired) {
		t.Errorf("YAML value at auth.pairing_state = %#v, want %q", value, PairingStateSetupRequired)
	}
	for _, path := range []string{"system.updates.exposed", "homekit.enabled"} {
		if value, _ := yamlPath(document, path); value != false {
			t.Errorf("YAML value at %q = %#v, want false", path, value)
		}
	}

	for _, path := range []string{"companion.device_id", "companion.model", "media", "sip"} {
		if _, ok := yamlPath(document, path); ok {
			t.Errorf("runtime YAML key path %q must not be persisted", path)
		}
	}
}

func yamlPath(document map[string]any, path string) (any, bool) {
	var value any = document
	for _, component := range strings.Split(path, ".") {
		switch current := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = current[component]
			if !ok {
				return nil, false
			}
		case []any:
			if component != "0" || len(current) == 0 {
				return nil, false
			}
			value = current[0]
		default:
			return nil, false
		}
	}

	return value, true
}

func TestCreateRejectsMissingMetadata(t *testing.T) {
	t.Parallel()

	_, err := Create(filepath.Join(t.TempDir(), "config.yaml"), Metadata{})
	if !errors.Is(err, ErrMissingMetadata) {
		t.Fatalf("create error = %v, want ErrMissingMetadata", err)
	}
}

func TestLoadRejectsUnsupportedCompanionMetadata(t *testing.T) {
	t.Parallel()

	for _, field := range []string{"model", "device_id"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if _, err := Create(path, Metadata{Model: testModel, MAC: testMAC}); err != nil {
				t.Fatalf("create config: %v", err)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read config: %v", err)
			}
			data = []byte(strings.Replace(string(data), "companion:\n", "companion:\n    "+field+": invalid\n", 1))
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			_, err = Load(path)
			if err == nil || !strings.Contains(err.Error(), "field "+field) {
				t.Fatalf("load error = %v, want unknown companion.%s field", err, field)
			}
		})
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
		cfg.Logging.Level = "debug"
		return nil
	}); err != nil {
		t.Fatalf("update store: %v", err)
	}

	if got := store.Snapshot().Logging.Level; got != "debug" {
		t.Fatalf("log level = %q, want debug", got)
	}

	if err := store.Update(func(cfg *Config) error {
		cfg.Companion.Entrypoints = []Entrypoint{}
		return nil
	}); err == nil {
		t.Fatal("invalid update succeeded")
	}

	if got := store.Snapshot().Logging.Level; got != "debug" {
		t.Fatalf("invalid update changed store: %q", got)
	}
}

func TestHomeKitPINGeneratedAndValidated(t *testing.T) {
	t.Parallel()

	cfg, err := Default(Metadata{Model: testModel, MAC: testMAC})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.HomeKit.PIN != "" || cfg.HomeKit.SetupID != "" {
		t.Fatalf("disabled HomeKit credentials = %#v, want empty", cfg.HomeKit)
	}
	if cfg.HomeKit.Name == "" {
		t.Fatalf("homekit runtime defaults = %#v, want name", cfg.HomeKit)
	}

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

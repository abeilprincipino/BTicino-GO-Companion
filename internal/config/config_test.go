package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	for component := range strings.SplitSeq(path, ".") {
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

func TestStoreUpdateKeepsStreamAndRingExclusive(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := Create(path, Metadata{Model: testModel, MAC: testMAC}); err != nil {
		t.Fatalf("create config: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	set := func(entrypoints ...Entrypoint) error {
		return store.Update(func(cfg *Config) error {
			cfg.Companion.Entrypoints = entrypoints
			return nil
		})
	}

	doorbell := Entrypoint{ID: "ingresso", Label: "Ingresso", DevAddr: "20", Capabilities: Capabilities{Stream: true, Unlock: true, Ring: true}}
	lights := Entrypoint{ID: "luci", Label: "Luci", DevAddr: "21", Capabilities: Capabilities{Unlock: true}}

	if err := set(doorbell, lights); err != nil {
		t.Fatalf("one doorbell entrypoint rejected: %v", err)
	}

	second := lights
	second.Capabilities = Capabilities{Stream: true, Ring: true}

	if err := set(doorbell, second); !errors.Is(err, ErrExclusiveCapability) {
		t.Fatalf("second doorbell: error = %v, want %v", err, ErrExclusiveCapability)
	}

	for _, capabilities := range []Capabilities{{Stream: true}, {Ring: true}} {
		split := lights
		split.Capabilities = capabilities

		if err := set(split); !errors.Is(err, ErrSplitDoorbell) {
			t.Fatalf("entrypoint with %+v: error = %v, want %v", capabilities, err, ErrSplitDoorbell)
		}
	}
}

func TestStoreUpdateMakesAddress20TheDoorbellWhenNoneStreams(t *testing.T) {
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
		cfg.Companion.Entrypoints = []Entrypoint{
			{ID: "luci", Label: "Luci", DevAddr: "21", Capabilities: Capabilities{Unlock: true}},
			{ID: "pappagallo", Label: "Ingresso", DevAddr: "20", Capabilities: Capabilities{Unlock: true}},
		}
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	entrypoints := store.Snapshot().Companion.Entrypoints
	if got := entrypoints[1].Capabilities; !got.Stream || !got.Ring || !got.Unlock {
		t.Fatalf("entrypoint at 20 capabilities = %+v, want stream, ring and unlock", got)
	}

	if got := entrypoints[0].Capabilities; got.Stream || got.Ring {
		t.Fatalf("entrypoint at 21 capabilities = %+v, want unlock only", got)
	}

	// Without an entrypoint at 20 nothing is assigned.
	if err := store.Update(func(cfg *Config) error {
		cfg.Companion.Entrypoints = []Entrypoint{{ID: "luci", Label: "Luci", DevAddr: "21", Capabilities: Capabilities{Unlock: true}}}
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := store.Snapshot().Companion.Entrypoints[0].Capabilities; got.Stream || got.Ring {
		t.Fatalf("capabilities = %+v, want unlock only", got)
	}
}

func TestDoorbellEntrypoint(t *testing.T) {
	t.Parallel()

	lock := Entrypoint{ID: "luci", DevAddr: "21", Capabilities: Capabilities{Unlock: true}}

	tests := []struct {
		name        string
		entrypoints []Entrypoint
		want        string
	}{
		{"streaming entrypoint, any name", []Entrypoint{lock, {ID: "pappagallo", Capabilities: Capabilities{Stream: true, Ring: true}}}, "pappagallo"},
		{"stream wins over ring", []Entrypoint{{ID: "a", Capabilities: Capabilities{Ring: true}}, {ID: "b", Capabilities: Capabilities{Stream: true}}}, "b"},
		{"ring when nothing streams", []Entrypoint{lock, {ID: "a", Capabilities: Capabilities{Ring: true}}}, "a"},
		{"two streams are ambiguous", []Entrypoint{{ID: "a", Capabilities: Capabilities{Stream: true}}, {ID: "b", Capabilities: Capabilities{Stream: true}}}, ""},
		{"locks only", []Entrypoint{lock}, ""},
	}

	for _, test := range tests {
		got, ok := DoorbellEntrypoint(test.entrypoints)
		if ok != (test.want != "") || got.ID != test.want {
			t.Errorf("%s: DoorbellEntrypoint() = %q/%t, want %q", test.name, got.ID, ok, test.want)
		}
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

func TestDefaultSIPSettings(t *testing.T) {
	t.Parallel()

	cfg, err := Default(Metadata{Model: "C300X", MAC: "aa:bb:cc:dd:ee:ff"})
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}

	sip := cfg.Companion.SIP
	if sip.From != "companion@127.0.0.1:5070" {
		t.Fatalf("From = %q, want companion@127.0.0.1:5070", sip.From)
	}

	if sip.Listen != "127.0.0.1:5070" {
		t.Fatalf("Listen = %q, want 127.0.0.1:5070", sip.Listen)
	}

	if sip.Transport != "tcp" {
		t.Fatalf("Transport = %q, want tcp", sip.Transport)
	}

	if sip.Inbound {
		t.Fatal("Inbound = true, want false by default")
	}
}

func TestSIPSettingsRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	if _, err := Create(path, Metadata{Model: "C300X", MAC: "aa:bb:cc:dd:ee:ff"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if err := store.Update(func(cfg *Config) error {
		cfg.Companion.SIP.Inbound = true
		cfg.Companion.SIP.From = "companion@127.0.0.1:5075"

		return nil
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !loaded.Companion.SIP.Inbound {
		t.Fatal("Inbound was not persisted")
	}

	if loaded.Companion.SIP.From != "companion@127.0.0.1:5075" {
		t.Fatalf("From = %q, want companion@127.0.0.1:5075", loaded.Companion.SIP.From)
	}
}

// legacyConfigWithoutSIP reproduces a config.yaml written before the
// companion.sip section existed. Every other value is deliberately non-default
// so a rewrite that dropped, reordered or defaulted a field would be visible.
const legacyConfigWithoutSIP = `companion:
    entrypoints:
        - id: main
          label: Main Gate
          devaddr: "20"
          capabilities:
            stream: true
            unlock: false
            ring: true
        - id: side
          label: Side Door
          devaddr: "21"
          capabilities:
            stream: true
            unlock: true
            ring: false
logging:
    level: debug
auth:
    home_assistant:
        pairing_state: claimed
        instance_id: 0123456789abcdef0123456789abcdef
        bearer_token_hash: "1111111111111111111111111111111111111111111111111111111111111111"
    webui:
        admin_username: admin
        admin_password_hash: "$2a$10$abcdefghijklmnopqrstuv"
        session_secret: "00112233445566778899aabbccddeeff"
system:
    reboot:
        enabled: true
    updates:
        enabled: false
        exposed: true
    services:
        companion:
            enabled: true
            exposed: false
        dropbear:
            enabled: false
            exposed: true
homekit:
    enabled: true
    pin: "123-45-679"
    setup_id: 7QWE
    name: Casa Rossi
`

// configWithSIPSection returns the same document with a companion.sip section,
// so both fixtures stay in sync.
func configWithSIPSection(t *testing.T) string {
	t.Helper()

	const section = `    sip:
        from: companion@127.0.0.1:5070
        auth_user: ""
        auth_pass: ""
        listen: 127.0.0.1:5070
        transport: tcp
        domain: ""
        inbound: false
`

	if !strings.Contains(legacyConfigWithoutSIP, "logging:\n") {
		t.Fatal("fixture no longer contains a logging section")
	}

	return strings.Replace(legacyConfigWithoutSIP, "logging:\n", section+"logging:\n", 1)
}

func TestSIPSectionPersistedReadsTheDocumentNotTheDefaults(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		document string
		want     bool
	}{
		"absent":  {document: legacyConfigWithoutSIP, want: false},
		"present": {document: configWithSIPSection(t), want: true},
		"partial": {
			document: strings.Replace(legacyConfigWithoutSIP, "logging:\n", "    sip:\n        inbound: true\nlogging:\n", 1),
			want:     true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(testCase.document), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			got, err := sipSectionPersisted(path)
			if err != nil {
				t.Fatalf("sipSectionPersisted() error = %v", err)
			}

			if got != testCase.want {
				t.Fatalf("sipSectionPersisted() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestLoadReadsThePersistedUpdateChannel(t *testing.T) {
	t.Parallel()

	const block = "    updates:\n        enabled: false\n"

	for name, testCase := range map[string]struct {
		channel string
		wantErr bool
	}{
		"beta":    {channel: "beta"},
		"unknown": {channel: "nightly", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if strings.Count(legacyConfigWithoutSIP, block) != 1 {
				t.Fatalf("updates block not found exactly once")
			}

			document := strings.Replace(legacyConfigWithoutSIP, block, block+"        channel: "+testCase.channel+"\n", 1)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			cfg, err := Load(path)
			if testCase.wantErr {
				if err == nil {
					t.Fatal("Load() error = nil, want error")
				}

				return
			}

			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}

			if cfg.System.UpdateChannel != "beta" {
				t.Fatalf("UpdateChannel = %q", cfg.System.UpdateChannel)
			}
		})
	}
}

func TestEnsureSIPSectionLeavesAnExistingSectionUntouched(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	before := []byte(configWithSIPSection(t))
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	migrated, err := store.EnsureSIPSection()
	if err != nil {
		t.Fatalf("EnsureSIPSection() error = %v", err)
	}

	if migrated {
		t.Fatal("EnsureSIPSection() = true, want false when the section is already persisted")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	if !bytes.Equal(before, after) {
		t.Fatalf("config was rewritten:\n%s", after)
	}
}

func TestEnsureSIPSectionPersistsAnAbsentSectionOnceWithInboundOff(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(legacyConfigWithoutSIP), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	migrated, err := store.EnsureSIPSection()
	if err != nil {
		t.Fatalf("EnsureSIPSection() error = %v", err)
	}

	if !migrated {
		t.Fatal("EnsureSIPSection() = false, want true when the section is absent")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode config: %v", err)
	}

	if value, ok := yamlPath(document, "companion.sip.inbound"); !ok || value != false {
		t.Fatalf("companion.sip.inbound = %#v (present=%v), want false", value, ok)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}

	// A second run must not rewrite the file: the section is now persisted.
	migrated, err = store.EnsureSIPSection()
	if err != nil {
		t.Fatalf("second EnsureSIPSection() error = %v", err)
	}

	if migrated {
		t.Fatal("second EnsureSIPSection() = true, want false")
	}

	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read config: %v", err)
	}

	if !bytes.Equal(data, again) {
		t.Fatalf("second run rewrote the config:\n%s", again)
	}
}

func TestEncodeAddsTheSIPSectionWithoutLosingPersistedValues(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	source := filepath.Join(directory, "config.yaml")

	if err := os.WriteFile(source, []byte(legacyConfigWithoutSIP), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	loaded, err := Load(source)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	data, err := encode(loaded)
	if err != nil {
		t.Fatalf("encode() error = %v", err)
	}

	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode encoded config: %v", err)
	}

	for path, want := range map[string]any{
		"companion.entrypoints.0.id":                  "main",
		"companion.entrypoints.0.label":               "Main Gate",
		"companion.entrypoints.0.devaddr":             "20",
		"companion.entrypoints.0.capabilities.stream": true,
		"companion.entrypoints.0.capabilities.unlock": false,
		"companion.entrypoints.0.capabilities.ring":   true,
		"companion.sip.from":                          "companion@127.0.0.1:5070",
		"companion.sip.listen":                        "127.0.0.1:5070",
		"companion.sip.transport":                     "tcp",
		"companion.sip.inbound":                       false,
		"logging.level":                               "debug",
		"auth.home_assistant.pairing_state":           "claimed",
		"auth.home_assistant.instance_id":             "0123456789abcdef0123456789abcdef",
		"auth.home_assistant.bearer_token_hash":       "1111111111111111111111111111111111111111111111111111111111111111",
		"auth.webui.admin_username":                   "admin",
		"auth.webui.admin_password_hash":              "$2a$10$abcdefghijklmnopqrstuv",
		"auth.webui.session_secret":                   "00112233445566778899aabbccddeeff",
		"system.reboot.enabled":                       true,
		"system.updates.enabled":                      false,
		"system.updates.exposed":                      true,
		"system.services.companion.enabled":           true,
		"system.services.companion.exposed":           false,
		"system.services.dropbear.enabled":            false,
		"system.services.dropbear.exposed":            true,
		"homekit.enabled":                             true,
		"homekit.pin":                                 "123-45-679",
		"homekit.setup_id":                            "7QWE",
		"homekit.name":                                "Casa Rossi",
	} {
		got, ok := yamlPath(document, path)
		if !ok {
			t.Errorf("missing YAML key path %q", path)

			continue
		}

		if got != want {
			t.Errorf("YAML value at %q = %#v, want %#v", path, got, want)
		}
	}

	target := filepath.Join(directory, "reencoded.yaml")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatalf("write encoded config: %v", err)
	}

	reloaded, err := Load(target)
	if err != nil {
		t.Fatalf("Load(re-encoded) error = %v", err)
	}

	if !reflect.DeepEqual(reloaded, loaded) {
		t.Fatalf("re-encoded config = %#v, want %#v", reloaded, loaded)
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

func TestUpdateChannelDefaultsToStableAndPersistsOnlyBeta(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(source, []byte(legacyConfigWithoutSIP), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	loaded, err := Load(source)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.System.UpdateChannel != UpdateChannelStable {
		t.Fatalf("UpdateChannel = %q", loaded.System.UpdateChannel)
	}

	for channel, want := range map[string]string{UpdateChannelStable: "", UpdateChannelBeta: UpdateChannelBeta} {
		loaded.System.UpdateChannel = channel

		data, err := encode(loaded)
		if err != nil {
			t.Fatalf("encode() error = %v", err)
		}

		var document struct {
			System struct {
				Updates map[string]any `yaml:"updates"`
			} `yaml:"system"`
		}
		if err := yaml.Unmarshal(data, &document); err != nil {
			t.Fatalf("decode encoded config: %v", err)
		}

		got, present := document.System.Updates["channel"]
		if want == "" && present || want != "" && got != want {
			t.Fatalf("channel %q encoded as %v (present %t)", channel, got, present)
		}
	}
}

func TestValidateUpdateChannel(t *testing.T) {
	t.Parallel()

	cfg, err := Default(Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.System.UpdateChannel != UpdateChannelStable {
		t.Fatalf("default UpdateChannel = %q", cfg.System.UpdateChannel)
	}

	for channel, valid := range map[string]bool{"": true, UpdateChannelStable: true, UpdateChannelBeta: true, "nightly": false} {
		cfg.System.UpdateChannel = channel
		if err := Validate(cfg); (err == nil) != valid {
			t.Fatalf("Validate(channel %q) error = %v", channel, err)
		}
	}
}

func TestUnlockSettingsLegacyAndRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(legacyConfigWithoutSIP), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("load legacy config: %v", err)
	}
	before := store.Snapshot()
	for _, entrypoint := range before.Companion.Entrypoints {
		if entrypoint.UnlockMode != "" || entrypoint.UnlockDurationMS != 0 {
			t.Fatalf("legacy unlock settings = %+v", entrypoint)
		}
	}
	encoded, err := encode(before)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("unlock_mode:")) || bytes.Contains(encoded, []byte("unlock_duration_ms:")) {
		t.Fatal("saving legacy settings introduced new unlock keys")
	}

	entrypoints := []Entrypoint{
		{ID: "pedonale", Label: "Pedonale", DevAddr: "20", Capabilities: Capabilities{Stream: true, Ring: true, Unlock: true}},
		{ID: "carrabile", Label: "Carrabile", DevAddr: "10", UnlockMode: UnlockModeAuxiliary, UnlockDurationMS: 1000, Capabilities: Capabilities{Unlock: true}},
	}
	if err := store.Update(func(cfg *Config) error {
		cfg.Companion.Entrypoints = entrypoints
		return nil
	}); err != nil {
		t.Fatalf("save unlock settings: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload unlock settings: %v", err)
	}
	if !reflect.DeepEqual(loaded.Companion.Entrypoints, entrypoints) {
		t.Fatalf("round-trip entrypoints = %+v, want %+v", loaded.Companion.Entrypoints, entrypoints)
	}
	if loaded.Auth != before.Auth || loaded.WebUI != before.WebUI {
		t.Fatal("saving unlock settings changed authentication")
	}
}

func TestValidateUnlockSettings(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		mode     string
		duration int
		valid    bool
	}{
		{"legacy defaults", "", 0, true},
		{"explicit door", UnlockModeDoor, 2000, true},
		{"auxiliary default duration", UnlockModeAuxiliary, 0, true},
		{"auxiliary one second", UnlockModeAuxiliary, 1000, true},
		{"minimum duration", UnlockModeAuxiliary, 1, true},
		{"unknown mode", "light", 1000, false},
		{"negative duration", UnlockModeAuxiliary, -1, false},
		{"excessive duration", UnlockModeAuxiliary, 2001, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if _, err := Create(path, Metadata{Model: testModel, MAC: testMAC}); err != nil {
				t.Fatal(err)
			}
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			before := store.Snapshot()
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			err = store.Update(func(cfg *Config) error {
				cfg.Companion.Entrypoints[0].UnlockMode = test.mode
				cfg.Companion.Entrypoints[0].UnlockDurationMS = test.duration
				return nil
			})
			if (err == nil) != test.valid {
				t.Fatalf("Update() error = %v, valid = %t", err, test.valid)
			}
			if !test.valid {
				persisted, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(persisted, original) || !reflect.DeepEqual(store.Snapshot(), before) {
					t.Fatal("invalid settings changed the file or in-memory configuration")
				}
			}
		})
	}
}

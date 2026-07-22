package config

import (
	"bticino-go-companion/internal/storage"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPath       = "/home/bticino/cfg/extra/companion/config.yaml"
	defaultLogLevel   = "info"
	defaultEntrypoint = "main"
)

var (
	ErrMissingMetadata   = errors.New("missing device metadata")
	ErrConfigExists      = errors.New("config already exists")
	ErrInvalidHomeKitPIN = errors.New("invalid homekit pin")
)

type Config struct {
	Companion Companion `yaml:"companion"`
	Auth      Auth      `yaml:"auth"`
	WebUI     WebUI     `yaml:"webui"`
	Logging   Logging   `yaml:"logging"`
	System    System    `yaml:"system"`
	HomeKit   HomeKit   `yaml:"homekit"`
}

type PairingState string

const (
	PairingStateSetupRequired PairingState = "setup_required"
	PairingStateClaimable     PairingState = "claimable"
	PairingStateClaimed       PairingState = "claimed"
	PairingStateError         PairingState = "error"
)

type Companion struct {
	DeviceID    string       `yaml:"-"`
	Model       string       `yaml:"-"`
	Entrypoints []Entrypoint `yaml:"entrypoints"`
}

type Entrypoint struct {
	ID           string       `yaml:"id" json:"id"`
	Label        string       `yaml:"label" json:"label"`
	DevAddr      string       `yaml:"devaddr" json:"devaddr"`
	Capabilities Capabilities `yaml:"capabilities" json:"capabilities"`
}

type Capabilities struct {
	Stream bool `yaml:"stream" json:"stream"`
	Unlock bool `yaml:"unlock" json:"unlock"`
	Ring   bool `yaml:"ring" json:"ring"`
}

type Auth struct {
	PairingState    PairingState `yaml:"pairing_state"`
	InstanceID      string       `yaml:"instance_id"`
	BearerTokenHash string       `yaml:"bearer_token_hash"`
}

type Logging struct {
	Level string `yaml:"level"`
}

type WebUI struct {
	AdminUsername     string `yaml:"admin_username"`
	AdminPasswordHash string `yaml:"admin_password_hash"`
	SessionSecret     string `yaml:"session_secret"`
}

type System struct {
	RebootEnabled bool               `yaml:"reboot_enabled"`
	UpdateEnabled bool               `yaml:"update_enabled"`
	UpdateExposed bool               `yaml:"update_exposed"`
	Services      map[string]Service `yaml:"services"`
}

type persistedAuth struct {
	HomeAssistant Auth  `yaml:"home_assistant"`
	WebUI         WebUI `yaml:"webui"`
}

type persistedSystem struct {
	Reboot struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"reboot"`
	Updates struct {
		Enabled bool `yaml:"enabled"`
		Exposed bool `yaml:"exposed"`
	} `yaml:"updates"`
	Services map[string]Service `yaml:"services"`
}

type Service struct {
	Enabled bool `yaml:"enabled"`
	Exposed bool `yaml:"exposed"`
}

type HomeKit struct {
	Enabled bool   `yaml:"enabled"`
	PIN     string `yaml:"pin"`
	SetupID string `yaml:"setup_id"`
	Name    string `yaml:"name"`
}

// persistedConfig defines the on-disk boundary. Device metadata is refreshed at
// startup and must never be written to config.yaml.
type persistedConfig struct {
	Companion persistedCompanion `yaml:"companion"`
	Logging   Logging            `yaml:"logging"`
	Auth      persistedAuth      `yaml:"auth"`
	System    persistedSystem    `yaml:"system"`
	HomeKit   HomeKit            `yaml:"homekit"`
}

type persistedCompanion struct {
	Entrypoints []Entrypoint `yaml:"entrypoints"`
}

type Metadata struct {
	Model string
	MAC   string
}

type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

func Create(path string, metadata Metadata) (Config, error) {
	if err := validateMetadata(metadata); err != nil {
		return Config{}, err
	}

	cfg, err := Default(metadata)
	if err != nil {
		return Config{}, err
	}

	if err := saveNew(path, cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func Open(path string) (*Store, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}

	return &Store{path: path, cfg: cfg}, nil
}

func Default(metadata Metadata) (Config, error) {
	if err := validateMetadata(metadata); err != nil {
		return Config{}, err
	}

	instanceID, err := RandomHex(16)
	if err != nil {
		return Config{}, fmt.Errorf("generate pairing instance id: %w", err)
	}

	cfg := Config{
		Companion: Companion{
			Entrypoints: []Entrypoint{{
				ID:      defaultEntrypoint,
				Label:   "Main Gate",
				DevAddr: "20",
				Capabilities: Capabilities{
					Stream: true,
					Unlock: true,
					Ring:   true,
				},
			}},
		},
		Auth: Auth{
			PairingState:    PairingStateSetupRequired,
			InstanceID:      instanceID,
			BearerTokenHash: "",
		},
		Logging: Logging{Level: defaultLogLevel},
		WebUI: WebUI{
			AdminUsername:     "",
			AdminPasswordHash: "",
			SessionSecret:     "",
		},
		System: System{
			RebootEnabled: true,
			UpdateEnabled: true,
			UpdateExposed: false,
			Services: map[string]Service{
				"companion": {Enabled: true, Exposed: true},
				"dropbear":  {Enabled: true, Exposed: true},
			},
		},
		HomeKit: HomeKit{
			Enabled: false,
			Name:    "BTicino Companion",
		},
	}
	if err := ApplyMetadata(&cfg, metadata); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// ApplyMetadata adds intercom facts discovered at runtime without persisting them.
func ApplyMetadata(cfg *Config, metadata Metadata) error {
	if err := validateMetadata(metadata); err != nil {
		return err
	}

	cfg.Companion.Model = metadata.Model
	cfg.Companion.DeviceID = strings.ToLower(strings.ReplaceAll(metadata.Model, " ", "-")) + "-" + strings.ReplaceAll(strings.ToLower(metadata.MAC), ":", "")

	return nil
}

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close() //nolint:errcheck // close error not meaningful for read-only handle

	var persisted persistedConfig

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)

	if err := decoder.Decode(&persisted); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	cfg := Config{
		Companion: Companion{
			Entrypoints: persisted.Companion.Entrypoints,
		},
		Auth:    persisted.Auth.HomeAssistant,
		WebUI:   persisted.Auth.WebUI,
		Logging: persisted.Logging,
		System:  System{RebootEnabled: persisted.System.Reboot.Enabled, UpdateEnabled: persisted.System.Updates.Enabled, UpdateExposed: persisted.System.Updates.Exposed, Services: persisted.System.Services},
		HomeKit: persisted.HomeKit,
	}
	if err := ensureSingleDocument(decoder); err != nil {
		return Config{}, err
	}

	if err := Validate(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (s *Store) Snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return clone(s.cfg)
}

func (s *Store) Update(update func(*Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := clone(s.cfg)
	if err := update(&next); err != nil {
		return err
	}

	if err := Validate(next); err != nil {
		return err
	}

	if err := save(s.path, next, false); err != nil {
		return err
	}

	s.cfg = next

	return nil
}

// ApplyMetadata adds intercom facts to the in-memory configuration only.
func (s *Store) ApplyMetadata(metadata Metadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return ApplyMetadata(&s.cfg, metadata)
}

func Validate(cfg Config) error {
	if cfg.Logging.Level != "debug" && cfg.Logging.Level != "info" && cfg.Logging.Level != "warn" && cfg.Logging.Level != "error" {
		return fmt.Errorf("invalid log level %q", cfg.Logging.Level)
	}

	if !validPairingState(cfg.Auth.PairingState) {
		return fmt.Errorf("invalid pairing state %q", cfg.Auth.PairingState)
	}
	if !validInstanceID(cfg.Auth.InstanceID) {
		return errors.New("pairing instance id must be a 32-character hexadecimal value")
	}
	if cfg.Auth.BearerTokenHash != "" && !validBearerTokenHash(cfg.Auth.BearerTokenHash) {
		return errors.New("bearer token hash must be a SHA-256 hexadecimal value")
	}
	if cfg.Auth.PairingState == PairingStateClaimed && cfg.Auth.BearerTokenHash == "" {
		return errors.New("claimed pairing state requires a bearer token hash")
	}
	if cfg.Auth.PairingState != PairingStateClaimed && cfg.Auth.BearerTokenHash != "" {
		return errors.New("unclaimed pairing state must not contain a bearer token hash")
	}

	if len(cfg.Companion.Entrypoints) == 0 {
		return errors.New("at least one entrypoint is required")
	}

	seen := map[string]struct{}{}

	for _, entrypoint := range cfg.Companion.Entrypoints {
		if strings.TrimSpace(entrypoint.ID) == "" || strings.TrimSpace(entrypoint.Label) == "" || strings.TrimSpace(entrypoint.DevAddr) == "" {
			return errors.New("entrypoint id, label, and devaddr are required")
		}

		if _, ok := seen[entrypoint.ID]; ok {
			return fmt.Errorf("duplicate entrypoint id %q", entrypoint.ID)
		}

		seen[entrypoint.ID] = struct{}{}
	}

	if cfg.HomeKit.PIN != "" && !validHomeKitPIN(cfg.HomeKit.PIN) {
		return ErrInvalidHomeKitPIN
	}
	return nil
}

func validateMetadata(metadata Metadata) error {
	if strings.TrimSpace(metadata.Model) == "" || strings.TrimSpace(metadata.MAC) == "" {
		return ErrMissingMetadata
	}

	return nil
}

func saveNew(path string, cfg Config) error {
	return save(path, cfg, true)
}

func save(path string, cfg Config, exclusive bool) error {
	data, err := yaml.Marshal(persistedConfig{
		Companion: persistedCompanion{
			Entrypoints: cfg.Companion.Entrypoints,
		},
		Logging: cfg.Logging,
		Auth:    persistedAuth{HomeAssistant: cfg.Auth, WebUI: cfg.WebUI},
		System: persistedSystem{Reboot: struct {
			Enabled bool `yaml:"enabled"`
		}{Enabled: cfg.System.RebootEnabled}, Updates: struct {
			Enabled bool `yaml:"enabled"`
			Exposed bool `yaml:"exposed"`
		}{Enabled: cfg.System.UpdateEnabled, Exposed: cfg.System.UpdateExposed}, Services: cfg.System.Services},
		HomeKit: cfg.HomeKit,
	})
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	if err := storage.WritePrivateFile(path, ".config-*.yaml", data, exclusive); err != nil {
		if exclusive && errors.Is(err, os.ErrExist) {
			return ErrConfigExists
		}
		return fmt.Errorf("save config: %w", err)
	}

	return nil
}

func ensureSingleDocument(decoder *yaml.Decoder) error {
	var extra any

	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("decode config document: %w", err)
	}

	return errors.New("config must contain one document")
}

func RandomHex(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}

	return hex.EncodeToString(bytes), nil
}

func GenerateClaimCode() (string, error) {
	first, err := RandomHex(2)
	if err != nil {
		return "", err
	}
	second, err := RandomHex(2)
	if err != nil {
		return "", err
	}
	return first + "-" + second, nil
}

func ValidClaimCode(code string) bool {
	if len(code) != 9 || code[4] != '-' {
		return false
	}
	_, err := hex.DecodeString(code[:4] + code[5:])
	return err == nil
}

func validBearerTokenHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validPairingState(state PairingState) bool {
	switch state {
	case PairingStateSetupRequired, PairingStateClaimable, PairingStateClaimed, PairingStateError:
		return true
	default:
		return false
	}
}

func validInstanceID(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func GenerateHomeKitPIN() (string, error) {
	for range 10 {
		value, err := rand.Int(rand.Reader, big.NewInt(100000000))
		if err != nil {
			return "", err
		}

		pin := fmt.Sprintf("%03d-%02d-%03d", value.Int64()/100000, value.Int64()/1000%100, value.Int64()%1000)
		if pin != "111-11-111" && pin != "123-45-678" {
			return pin, nil
		}
	}

	return "", errors.New("failed to generate homekit pin after 10 attempts")
}

func GenerateHomeKitSetupID() (string, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

	setupID := make([]byte, 4)
	for i := range setupID {
		value, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		setupID[i] = alphabet[value.Int64()]
	}

	return string(setupID), nil
}

func validHomeKitPIN(pin string) bool {
	if len(pin) != 10 || pin[3] != '-' || pin[6] != '-' {
		return false
	}

	for i := range pin {
		if i == 3 || i == 6 {
			continue
		}

		if pin[i] < '0' || pin[i] > '9' {
			return false
		}
	}

	return true
}

func clone(cfg Config) Config {
	cfgCopy := cfg
	cfgCopy.Companion.Entrypoints = append([]Entrypoint{}, cfg.Companion.Entrypoints...)

	cfgCopy.System.Services = make(map[string]Service, len(cfg.System.Services))
	maps.Copy(cfgCopy.System.Services, cfg.System.Services)

	return cfgCopy
}

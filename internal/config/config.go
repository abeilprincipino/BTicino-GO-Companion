package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	ErrMissingMetadata = errors.New("missing device metadata")
	ErrConfigExists    = errors.New("config already exists")
)

type Config struct {
	Companion Companion `yaml:"companion"`
	Auth      Auth      `yaml:"auth"`
	WebUI     WebUI     `yaml:"webui"`
	System    System    `yaml:"system"`
	HomeKit   HomeKit   `yaml:"homekit"`
}

type Companion struct {
	Name        string       `yaml:"name"`
	LogLevel    string       `yaml:"log_level"`
	DeviceID    string       `yaml:"device_id"`
	Model       string       `yaml:"model"`
	Entrypoints []Entrypoint `yaml:"entrypoints"`
}

type Entrypoint struct {
	ID           string       `yaml:"id"`
	Label        string       `yaml:"label"`
	DevAddr      string       `yaml:"devaddr"`
	Capabilities Capabilities `yaml:"capabilities"`
}

type Capabilities struct {
	Stream bool `yaml:"stream"`
	Unlock bool `yaml:"unlock"`
	Ring   bool `yaml:"ring"`
}

type Auth struct {
	ClaimCode   string `yaml:"claim_code"`
	BearerToken string `yaml:"bearer_token"`
}

type WebUI struct {
	AdminPasswordHash string `yaml:"admin_password_hash"`
	SessionSecret     string `yaml:"session_secret"`
}

type System struct {
	RebootEnabled bool               `yaml:"reboot_enabled"`
	UpdateEnabled bool               `yaml:"update_enabled"`
	UpdateExposed bool               `yaml:"update_exposed"`
	AllowRollback bool               `yaml:"allow_rollback"`
	Services      map[string]Service `yaml:"services"`
}

type Service struct {
	Enabled bool `yaml:"enabled"`
	Exposed bool `yaml:"exposed"`
}

type HomeKit struct {
	Enabled bool   `yaml:"enabled"`
	PIN     string `yaml:"pin"`
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
	claimCode, err := randomHex(4)
	if err != nil {
		return Config{}, fmt.Errorf("generate claim code: %w", err)
	}

	deviceID := strings.ToLower(strings.ReplaceAll(metadata.Model, " ", "-")) + "-" + strings.ReplaceAll(strings.ToLower(metadata.MAC), ":", "")
	return Config{
		Companion: Companion{
			Name:     "BTicino Companion",
			LogLevel: defaultLogLevel,
			DeviceID: deviceID,
			Model:    metadata.Model,
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
		Auth: Auth{ClaimCode: claimCode},
		System: System{
			RebootEnabled: true,
			UpdateEnabled: true,
			Services: map[string]Service{
				"dropbear": {Enabled: true, Exposed: true},
			},
		},
	}, nil
}

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	var cfg Config
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
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

func Validate(cfg Config) error {
	if strings.TrimSpace(cfg.Companion.Name) == "" {
		return errors.New("companion name is required")
	}
	if cfg.Companion.LogLevel != "debug" && cfg.Companion.LogLevel != "info" && cfg.Companion.LogLevel != "warn" && cfg.Companion.LogLevel != "error" {
		return fmt.Errorf("invalid log level %q", cfg.Companion.LogLevel)
	}
	if strings.TrimSpace(cfg.Companion.DeviceID) == "" || strings.TrimSpace(cfg.Companion.Model) == "" {
		return ErrMissingMetadata
	}
	if len(cfg.Auth.ClaimCode) != 8 {
		return errors.New("claim code must be 8 hex characters")
	}
	if _, err := hex.DecodeString(cfg.Auth.ClaimCode); err != nil {
		return errors.New("claim code must be hexadecimal")
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
	return nil
}

func validateMetadata(metadata Metadata) error {
	if strings.TrimSpace(metadata.Model) == "" || strings.TrimSpace(metadata.MAC) == "" {
		return ErrMissingMetadata
	}
	return nil
}

func saveNew(path string, cfg Config) error {
	if _, err := os.Stat(path); err == nil {
		return ErrConfigExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat config: %w", err)
	}
	return save(path, cfg, true)
}

func save(path string, cfg Config, exclusive bool) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary config mode: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if exclusive {
		if _, err := os.Stat(path); err == nil {
			return ErrConfigExists
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat config before create: %w", err)
		}
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
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

func randomHex(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func clone(cfg Config) Config {
	copy := cfg
	copy.Companion.Entrypoints = append([]Entrypoint{}, cfg.Companion.Entrypoints...)
	copy.System.Services = make(map[string]Service, len(cfg.System.Services))
	for name, service := range cfg.System.Services {
		copy.System.Services[name] = service
	}
	return copy
}

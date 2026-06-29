package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bticino-go-companion/internal/domain/entrypoint"
)

const SchemaVersion = 2

var (
	BuildVersion     = "0.1.0-dev"
	BuildGitSHA      = "dev"
	BuildDate        = "unknown"
	BuildReleaseRepo = ""
)

type SystemServiceConfig struct {
	Enabled bool `json:"enabled"`
	Exposed bool `json:"exposed"`
}

type Config struct {
	SchemaVersion             int
	Version                   string
	GitSHA                    string
	BuildDate                 string
	ListenAddr                string
	DataDir                   string
	ClaimCode                 string
	DeviceName                string
	DeviceModel               string
	DeviceFirmware            string
	DeviceHardware            string
	DeviceKernel              string
	DeviceDistribution        string
	DeviceIP                  string
	DeviceNetmask             string
	DeviceMAC                 string
	MDNSEnabled               bool
	MDNSServiceType           string
	OpenWebNetEnabled         bool
	OpenWebNetGroup           string
	OpenWebNetListenPort      int
	OpenWebNetReadBuffer      int
	OpenWebNetCommandHost     string
	OpenWebNetCommandPort     int
	OpenWebNetCommandSec      int
	OpenWebNetCommandPassword string
	MediaSIPEnabled           bool
	MediaSIPTransport         string
	MediaSIPListen            string
	MediaSIPFrom              string
	MediaSIPTo                string
	MediaSIPDomain            string
	MediaSIPAuthUser          string
	MediaSIPAuthPass          string
	MediaRTSPEnabled          bool
	MediaRTSPAddress          string
	MediaRTPAudioPort         int
	MediaRTPVideoPort         int
	VoicemailMessagesDir      string

	SystemRebootEnabled       bool
	SystemUpdateEnabled       bool
	SystemUpdateExposed       bool
	SystemUpdateAllowRollback bool
	SystemServices            map[string]SystemServiceConfig
	UpdateManifestPath        string
	UpdateReleaseAPI          string
	UpdateReleaseRepo         string
	UpdateReleaseAsset        string
	UpdateServiceScript       string
	UpdateHealthTimeoutSec    int
	MuteEnabled               bool
	ExposeMuteControl         bool
	VoicemailEnabled          bool
	ExposeVoicemailToggle     bool

	Auth        AuthState
	Entrypoints []entrypoint.Model
}

type AuthState struct {
	DeviceID    string `json:"device_id"`
	Claimed     bool   `json:"claimed"`
	ClaimCode   string `json:"claim_code"`
	BearerToken string `json:"bearer_token"`
	KeyID       string `json:"key_id"`
}

type PersistedConfig struct {
	SchemaVersion             int                `json:"schema_version"`
	System                    PersistedSystem    `json:"system"`
	Companion                 PersistedCompanion `json:"companion"`
	OpenWebNetCommandPassword string             `json:"openwebnet_command_password,omitempty"`
}

type PersistedSystem struct {
	Control  PersistedSystemControl            `json:"control"`
	Services map[string]PersistedSystemService `json:"services,omitempty"`
	Future   map[string]any                    `json:"future,omitempty"`
}

type PersistedSystemControl struct {
	Reboot PersistedSystemReboot `json:"reboot"`
	Update PersistedSystemUpdate `json:"update"`
}

type PersistedSystemReboot struct {
	Enabled *bool `json:"enabled,omitempty"`
}

type PersistedSystemUpdate struct {
	Enabled       *bool  `json:"enabled,omitempty"`
	Exposed       *bool  `json:"exposed,omitempty"`
	AllowRollback *bool  `json:"allow_rollback,omitempty"`
	ManifestPath  string `json:"manifest_path,omitempty"`
	ReleaseAPI    string `json:"release_api,omitempty"`
	ReleaseRepo   string `json:"release_repo,omitempty"`
	ReleaseAsset  string `json:"release_asset,omitempty"`
	ServiceScript string `json:"service_script,omitempty"`
	HealthTimeout *int   `json:"health_timeout_sec,omitempty"`
}

type PersistedSystemService struct {
	Enabled *bool `json:"enabled,omitempty"`
	Exposed *bool `json:"exposed,omitempty"`
}

type PersistedCompanion struct {
	Info   PersistedCompanionInfo   `json:"info"`
	Auth   AuthState                `json:"auth"`
	Config PersistedCompanionConfig `json:"config"`
}

type PersistedCompanionInfo struct {
	Model string `json:"model,omitempty"`
}

type PersistedCompanionConfig struct {
	Entrypoints []entrypoint.Model        `json:"entrypoints"`
	Audio       PersistedCompanionAudio   `json:"audio"`
	Voicemail   PersistedCompanionMailbox `json:"voicemail"`
}

type PersistedCompanionAudio struct {
	Enabled *bool `json:"enabled,omitempty"`
	Exposed *bool `json:"exposed,omitempty"`
}

type PersistedCompanionMailbox struct {
	MessagesDir string `json:"messages_dir,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
	Exposed     *bool  `json:"exposed,omitempty"`
}

func Default() Config {
	return Config{
		SchemaVersion:             SchemaVersion,
		Version:                   BuildVersion,
		GitSHA:                    BuildGitSHA,
		BuildDate:                 BuildDate,
		ListenAddr:                "0.0.0.0:8080",
		DataDir:                   "/home/bticino/cfg/extra/companion",
		ClaimCode:                 "",
		DeviceName:                "BTicino Companion",
		DeviceModel:               "unknown",
		DeviceFirmware:            "unknown",
		DeviceHardware:            "unknown",
		DeviceKernel:              "unknown",
		DeviceDistribution:        "unknown",
		DeviceIP:                  "",
		DeviceNetmask:             "",
		DeviceMAC:                 "",
		MDNSEnabled:               true,
		MDNSServiceType:           "_bticomp._tcp",
		OpenWebNetEnabled:         true,
		OpenWebNetGroup:           "239.255.76.67",
		OpenWebNetListenPort:      7667,
		OpenWebNetReadBuffer:      65535,
		OpenWebNetCommandHost:     "127.0.0.1",
		OpenWebNetCommandPort:     20000,
		OpenWebNetCommandSec:      3,
		OpenWebNetCommandPassword: "",
		MediaSIPEnabled:           true,
		MediaSIPTransport:         "tcp",
		MediaSIPListen:            "0.0.0.0:5070",
		MediaSIPFrom:              "webrtc@127.0.0.1",
		MediaSIPTo:                "",
		MediaSIPDomain:            "",
		MediaSIPAuthUser:          "",
		MediaSIPAuthPass:          "",
		MediaRTSPEnabled:          true,
		MediaRTSPAddress:          ":8554",
		MediaRTPAudioPort:         5000,
		MediaRTPVideoPort:         5007,
		VoicemailMessagesDir:      "/home/bticino/cfg/extra/47/messages",
		SystemRebootEnabled:       true,
		SystemUpdateEnabled:       true,
		SystemUpdateExposed:       false,
		SystemUpdateAllowRollback: false,
		SystemServices: map[string]SystemServiceConfig{
			"dropbear": {
				Enabled: true,
				Exposed: true,
			},
		},
		UpdateManifestPath:     "",
		UpdateReleaseAPI:       "https://api.github.com",
		UpdateReleaseRepo:      strings.TrimSpace(BuildReleaseRepo),
		UpdateReleaseAsset:     "companion.tar.gz",
		UpdateServiceScript:    "/etc/init.d/companion",
		UpdateHealthTimeoutSec: 8,
		MuteEnabled:            true,
		ExposeMuteControl:      true,
		VoicemailEnabled:       true,
		ExposeVoicemailToggle:  true,
		Entrypoints: []entrypoint.Model{
			{
				ID:        "main",
				Label:     "Main Gate",
				DevAddr:   "20",
				HasStream: true,
				HasUnlock: true,
				HasRing:   true,
			},
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	path = strings.TrimSpace(path)
	if path == "" {
		cfg.normalize()
		return cfg, nil
	}

	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg.normalize()
			return cfg, nil
		}
		return Config{}, err
	}

	var persisted PersistedConfig
	if err := json.Unmarshal(b, &persisted); err != nil {
		return Config{}, fmt.Errorf("parse persisted config: %w", err)
	}

	if persisted.SchemaVersion > 0 {
		cfg.SchemaVersion = persisted.SchemaVersion
	}
	cfg.OpenWebNetCommandPassword = strings.TrimSpace(persisted.OpenWebNetCommandPassword)

	cfg.DeviceModel = strings.TrimSpace(persisted.Companion.Info.Model)
	cfg.Auth = persisted.Companion.Auth
	cfg.ClaimCode = strings.TrimSpace(cfg.Auth.ClaimCode)

	cfg.SystemRebootEnabled = boolFromPtr(persisted.System.Control.Reboot.Enabled, cfg.SystemRebootEnabled)
	cfg.SystemUpdateEnabled = boolFromPtr(persisted.System.Control.Update.Enabled, cfg.SystemUpdateEnabled)
	cfg.SystemUpdateExposed = boolFromPtr(persisted.System.Control.Update.Exposed, cfg.SystemUpdateExposed)
	cfg.SystemUpdateAllowRollback = boolFromPtr(persisted.System.Control.Update.AllowRollback, cfg.SystemUpdateAllowRollback)
	if strings.TrimSpace(persisted.System.Control.Update.ManifestPath) != "" {
		cfg.UpdateManifestPath = strings.TrimSpace(persisted.System.Control.Update.ManifestPath)
	}
	if strings.TrimSpace(persisted.System.Control.Update.ReleaseAPI) != "" {
		cfg.UpdateReleaseAPI = strings.TrimSpace(persisted.System.Control.Update.ReleaseAPI)
	}
	if strings.TrimSpace(persisted.System.Control.Update.ReleaseRepo) != "" {
		cfg.UpdateReleaseRepo = strings.TrimSpace(persisted.System.Control.Update.ReleaseRepo)
	}
	if strings.TrimSpace(persisted.System.Control.Update.ReleaseAsset) != "" {
		cfg.UpdateReleaseAsset = strings.TrimSpace(persisted.System.Control.Update.ReleaseAsset)
	}
	if strings.TrimSpace(persisted.System.Control.Update.ServiceScript) != "" {
		cfg.UpdateServiceScript = strings.TrimSpace(persisted.System.Control.Update.ServiceScript)
	}
	if persisted.System.Control.Update.HealthTimeout != nil && *persisted.System.Control.Update.HealthTimeout > 0 {
		cfg.UpdateHealthTimeoutSec = *persisted.System.Control.Update.HealthTimeout
	}
	if len(persisted.System.Services) > 0 {
		cfg.SystemServices = make(map[string]SystemServiceConfig, len(persisted.System.Services))
		for rawName, rawCfg := range persisted.System.Services {
			name := normalizeName(rawName)
			if name == "" {
				continue
			}
			cfg.SystemServices[name] = SystemServiceConfig{
				Enabled: boolFromPtr(rawCfg.Enabled, true),
				Exposed: boolFromPtr(rawCfg.Exposed, false),
			}
		}
	}

	if len(persisted.Companion.Config.Entrypoints) > 0 {
		cfg.Entrypoints = persisted.Companion.Config.Entrypoints
	}
	cfg.MuteEnabled = boolFromPtr(persisted.Companion.Config.Audio.Enabled, cfg.MuteEnabled)
	cfg.ExposeMuteControl = boolFromPtr(persisted.Companion.Config.Audio.Exposed, cfg.ExposeMuteControl)
	if strings.TrimSpace(persisted.Companion.Config.Voicemail.MessagesDir) != "" {
		cfg.VoicemailMessagesDir = strings.TrimSpace(persisted.Companion.Config.Voicemail.MessagesDir)
	}
	cfg.VoicemailEnabled = boolFromPtr(persisted.Companion.Config.Voicemail.Enabled, cfg.VoicemailEnabled)
	cfg.ExposeVoicemailToggle = boolFromPtr(persisted.Companion.Config.Voicemail.Exposed, cfg.ExposeVoicemailToggle)

	cfg.normalize()
	return cfg, nil
}

func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	cfg.normalize()

	persistedServices := make(map[string]PersistedSystemService, len(cfg.SystemServices))
	for name, sc := range cfg.SystemServices {
		persistedServices[name] = PersistedSystemService{
			Enabled: boolPtr(sc.Enabled),
			Exposed: boolPtr(sc.Exposed),
		}
	}

	persisted := PersistedConfig{
		SchemaVersion:             SchemaVersion,
		OpenWebNetCommandPassword: strings.TrimSpace(cfg.OpenWebNetCommandPassword),
		System: PersistedSystem{
			Control: PersistedSystemControl{
				Reboot: PersistedSystemReboot{Enabled: boolPtr(cfg.SystemRebootEnabled)},
				Update: PersistedSystemUpdate{
					Enabled:       boolPtr(cfg.SystemUpdateEnabled),
					Exposed:       boolPtr(cfg.SystemUpdateExposed),
					AllowRollback: boolPtr(cfg.SystemUpdateAllowRollback),
					ManifestPath:  strings.TrimSpace(cfg.UpdateManifestPath),
					ReleaseAPI:    strings.TrimSpace(cfg.UpdateReleaseAPI),
					ReleaseRepo:   strings.TrimSpace(cfg.UpdateReleaseRepo),
					ReleaseAsset:  strings.TrimSpace(cfg.UpdateReleaseAsset),
					ServiceScript: strings.TrimSpace(cfg.UpdateServiceScript),
					HealthTimeout: intPtrPositive(cfg.UpdateHealthTimeoutSec),
				},
			},
			Services: persistedServices,
			Future:   map[string]any{},
		},
		Companion: PersistedCompanion{
			Info: PersistedCompanionInfo{
				Model: strings.TrimSpace(cfg.DeviceModel),
			},
			Auth: configAuthState(cfg),
			Config: PersistedCompanionConfig{
				Entrypoints: cfg.Entrypoints,
				Audio: PersistedCompanionAudio{
					Enabled: boolPtr(cfg.MuteEnabled),
					Exposed: boolPtr(cfg.ExposeMuteControl),
				},
				Voicemail: PersistedCompanionMailbox{
					MessagesDir: strings.TrimSpace(cfg.VoicemailMessagesDir),
					Enabled:     boolPtr(cfg.VoicemailEnabled),
					Exposed:     boolPtr(cfg.ExposeVoicemailToggle),
				},
			},
		},
	}

	b, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func ResolvePath(path string) (string, error) {
	raw := strings.TrimSpace(path)
	if raw != "" {
		return raw, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), "config.json"), nil
}

func (c Config) UpdateBinCurrentPath() string {
	return filepath.Join(c.DataDir, "companion")
}

func (c Config) UpdateBinPreviousPath() string {
	return filepath.Join(c.DataDir, "companion.previous")
}

func (c Config) UpdateBinCandidatePath() string {
	return filepath.Join(c.DataDir, "companion.candidate")
}

func (c *Config) normalize() {
	if c.SchemaVersion <= 0 {
		c.SchemaVersion = SchemaVersion
	}
	c.Version = strings.TrimSpace(c.Version)
	c.GitSHA = strings.TrimSpace(c.GitSHA)
	c.BuildDate = strings.TrimSpace(c.BuildDate)
	if c.Version == "" {
		c.Version = BuildVersion
	}
	if c.GitSHA == "" {
		c.GitSHA = BuildGitSHA
	}
	if c.BuildDate == "" {
		c.BuildDate = BuildDate
	}
	if strings.TrimSpace(c.ListenAddr) == "" {
		c.ListenAddr = "0.0.0.0:8080"
	}
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = "/home/bticino/cfg/extra/companion"
	}
	c.ClaimCode = strings.TrimSpace(c.ClaimCode)
	c.DeviceName = strings.TrimSpace(c.DeviceName)
	c.DeviceModel = strings.TrimSpace(c.DeviceModel)
	c.DeviceFirmware = strings.TrimSpace(c.DeviceFirmware)
	c.DeviceHardware = strings.TrimSpace(c.DeviceHardware)
	c.DeviceKernel = strings.TrimSpace(c.DeviceKernel)
	c.DeviceDistribution = strings.TrimSpace(c.DeviceDistribution)
	c.DeviceIP = strings.TrimSpace(c.DeviceIP)
	c.DeviceNetmask = strings.TrimSpace(c.DeviceNetmask)
	c.DeviceMAC = strings.TrimSpace(c.DeviceMAC)
	c.MDNSServiceType = strings.TrimSpace(c.MDNSServiceType)
	c.Auth.DeviceID = strings.TrimSpace(c.Auth.DeviceID)
	c.Auth.ClaimCode = strings.TrimSpace(c.Auth.ClaimCode)
	if c.ClaimCode == "" && c.Auth.ClaimCode != "" {
		c.ClaimCode = c.Auth.ClaimCode
	}
	if c.Auth.ClaimCode == "" && c.ClaimCode != "" {
		c.Auth.ClaimCode = c.ClaimCode
	}
	c.Auth.BearerToken = strings.TrimSpace(c.Auth.BearerToken)
	c.Auth.KeyID = strings.TrimSpace(c.Auth.KeyID)
	if c.DeviceName == "" {
		c.DeviceName = "BTicino Companion"
	}
	if c.DeviceModel == "" {
		c.DeviceModel = "unknown"
	}
	if c.DeviceFirmware == "" {
		c.DeviceFirmware = "unknown"
	}
	if c.DeviceHardware == "" {
		c.DeviceHardware = "unknown"
	}
	if c.DeviceKernel == "" {
		c.DeviceKernel = "unknown"
	}
	if c.DeviceDistribution == "" {
		c.DeviceDistribution = "unknown"
	}
	if c.MDNSServiceType == "" {
		c.MDNSServiceType = "_bticomp._tcp"
	}
	if strings.TrimSpace(c.OpenWebNetGroup) == "" {
		c.OpenWebNetGroup = "239.255.76.67"
	}
	if c.OpenWebNetListenPort <= 0 {
		c.OpenWebNetListenPort = 7667
	}
	if c.OpenWebNetReadBuffer <= 0 {
		c.OpenWebNetReadBuffer = 65535
	}
	if strings.TrimSpace(c.OpenWebNetCommandHost) == "" {
		c.OpenWebNetCommandHost = "127.0.0.1"
	}
	if c.OpenWebNetCommandPort <= 0 {
		c.OpenWebNetCommandPort = 20000
	}
	if c.OpenWebNetCommandSec <= 0 {
		c.OpenWebNetCommandSec = 3
	}
	c.OpenWebNetCommandPassword = strings.TrimSpace(c.OpenWebNetCommandPassword)
	c.VoicemailMessagesDir = strings.TrimSpace(c.VoicemailMessagesDir)
	c.UpdateManifestPath = strings.TrimSpace(c.UpdateManifestPath)
	c.UpdateReleaseAPI = strings.TrimSpace(c.UpdateReleaseAPI)
	c.UpdateReleaseRepo = strings.TrimSpace(c.UpdateReleaseRepo)
	c.UpdateReleaseAsset = strings.TrimSpace(c.UpdateReleaseAsset)
	c.UpdateServiceScript = strings.TrimSpace(c.UpdateServiceScript)
	if strings.TrimSpace(c.MediaSIPTransport) == "" {
		c.MediaSIPTransport = "tcp"
	}
	if strings.TrimSpace(c.MediaSIPListen) == "" {
		c.MediaSIPListen = "0.0.0.0:5070"
	}
	if strings.TrimSpace(c.MediaSIPFrom) == "" {
		c.MediaSIPFrom = "webrtc@127.0.0.1"
	}
	if strings.TrimSpace(c.MediaRTSPAddress) == "" {
		c.MediaRTSPAddress = ":8554"
	}
	if c.MediaRTPAudioPort <= 0 || c.MediaRTPAudioPort > 65535 {
		c.MediaRTPAudioPort = 5000
	}
	if c.MediaRTPVideoPort <= 0 || c.MediaRTPVideoPort > 65535 {
		c.MediaRTPVideoPort = 5007
	}
	if c.VoicemailMessagesDir == "" {
		c.VoicemailMessagesDir = "/home/bticino/cfg/extra/47/messages"
	}
	if c.UpdateReleaseAPI == "" {
		c.UpdateReleaseAPI = "https://api.github.com"
	}
	if c.UpdateReleaseAsset == "" || c.UpdateReleaseAsset == "companion" {
		c.UpdateReleaseAsset = "companion.tar.gz"
	}
	if c.UpdateServiceScript == "" {
		c.UpdateServiceScript = "/etc/init.d/companion"
	}
	if c.UpdateHealthTimeoutSec <= 0 {
		c.UpdateHealthTimeoutSec = 8
	}
	if len(c.Entrypoints) == 0 {
		c.Entrypoints = Default().Entrypoints
	}
	for i := range c.Entrypoints {
		ep := &c.Entrypoints[i]
		ep.ID = strings.TrimSpace(ep.ID)
		ep.Label = strings.TrimSpace(ep.Label)
		ep.DevAddr = strings.TrimSpace(ep.DevAddr)
		if ep.ID == "" {
			ep.ID = "main"
		}
		if ep.Label == "" {
			ep.Label = ep.ID
		}
		if ep.DevAddr == "" {
			ep.DevAddr = "20"
		}
	}

	c.SystemServices = normalizeSystemServices(c.SystemServices)
	if len(c.SystemServices) == 0 {
		c.SystemServices = map[string]SystemServiceConfig{
			"dropbear": {Enabled: true, Exposed: true},
		}
	}
	if !c.SystemUpdateEnabled {
		c.SystemUpdateExposed = false
		c.SystemUpdateAllowRollback = false
	}
	if !c.SystemUpdateExposed {
		c.SystemUpdateAllowRollback = false
	}

	if !c.MuteEnabled {
		c.ExposeMuteControl = false
	}
	if !c.VoicemailEnabled {
		c.ExposeVoicemailToggle = false
	}
	if strings.EqualFold(c.DeviceModel, "C100X") {
		c.VoicemailEnabled = false
		c.ExposeVoicemailToggle = false
	}
}

// ResolveDefaultStreamDevAddr returns the SIP SDP DEVADDR for stream setup.
// C100X stores the video-door-entry module id separately from the lock address.
func ResolveDefaultStreamDevAddr(deviceModel string, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if strings.EqualFold(strings.TrimSpace(deviceModel), "C100X") {
		if devAddr := detectC100XStreamDevAddr(); devAddr != "" {
			return devAddr
		}
	}
	if fallback != "" {
		return fallback
	}
	return "20"
}

func configAuthState(cfg Config) AuthState {
	auth := cfg.Auth
	auth.ClaimCode = strings.TrimSpace(auth.ClaimCode)
	if auth.ClaimCode == "" {
		auth.ClaimCode = strings.TrimSpace(cfg.ClaimCode)
	}
	return auth
}

func normalizeSystemServices(raw map[string]SystemServiceConfig) map[string]SystemServiceConfig {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]SystemServiceConfig, len(raw))
	for name, cfg := range raw {
		normalized := normalizeName(name)
		if normalized == "" {
			continue
		}
		out[normalized] = SystemServiceConfig{Enabled: cfg.Enabled, Exposed: cfg.Exposed}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeName(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

var c100xModulesPath = "/home/bticino/cfg/extra/.bt_eliot/mymodules"

type c100xModule struct {
	ID             string `json:"id"`
	System         string `json:"system"`
	DeviceType     string `json:"deviceType"`
	PrivateAddress struct {
		AddressValues []struct {
			Value string `json:"value"`
		} `json:"addressValues"`
	} `json:"privateAddress"`
}

type c100xModulesFile struct {
	Modules []c100xModule `json:"modules"`
}

func detectC100XStreamDevAddr() string {
	b, err := os.ReadFile(c100xModulesPath)
	if err != nil {
		return ""
	}
	var file c100xModulesFile
	if err := json.Unmarshal(b, &file); err != nil {
		return ""
	}
	var matches []string
	for _, m := range file.Modules {
		if !strings.EqualFold(m.System, "videodoorentry") || !strings.EqualFold(m.DeviceType, "EU") {
			continue
		}
		has20 := false
		for _, av := range m.PrivateAddress.AddressValues {
			if strings.TrimSpace(av.Value) == "20" {
				has20 = true
				break
			}
		}
		if has20 {
			if id := strings.TrimSpace(m.ID); id != "" {
				matches = append(matches, id)
			}
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

func boolPtr(v bool) *bool {
	b := v
	return &b
}

func boolFromPtr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func intPtrPositive(v int) *int {
	if v <= 0 {
		return nil
	}
	value := v
	return &value
}

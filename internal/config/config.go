package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bticino-go-companion/internal/domain/entrypoint"
)

const SchemaVersion = 2

type SystemServiceConfig struct {
	Enabled bool `json:"enabled"`
	Exposed bool `json:"exposed"`
}

type Config struct {
	SchemaVersion             int
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
	DeviceWiFiRSSI            *int
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
	MediaRTSPPathMain         string
	MediaRTPAudioPort         int
	MediaRTPVideoPort         int
	VoicemailMessagesDir      string

	SystemRebootEnabled   bool
	SystemServices        map[string]SystemServiceConfig
	MuteEnabled           bool
	ExposeMuteControl     bool
	VoicemailEnabled      bool
	ExposeVoicemailToggle bool

	Auth        AuthState
	Entrypoints []entrypoint.Model
}

type AuthState struct {
	DeviceID              string    `json:"device_id"`
	Claimed               bool      `json:"claimed"`
	ClaimCode             string    `json:"claim_code"`
	BearerToken           string    `json:"bearer_token"`
	KeyID                 string    `json:"key_id"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at,omitempty"`
	RefreshToken          string    `json:"refresh_token,omitempty"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at,omitempty"`
}

type PersistedConfig struct {
	SchemaVersion             int               `json:"schema_version"`
	System                    PersistedSystem   `json:"system"`
	Intercom                  PersistedIntercom `json:"intercom"`
	OpenWebNetCommandPassword string            `json:"openwebnet_command_password,omitempty"`
}

type PersistedSystem struct {
	Control  PersistedSystemControl            `json:"control"`
	Services map[string]PersistedSystemService `json:"services,omitempty"`
	Future   map[string]any                    `json:"future,omitempty"`
}

type PersistedSystemControl struct {
	Reboot PersistedSystemReboot `json:"reboot"`
}

type PersistedSystemReboot struct {
	Enabled *bool `json:"enabled,omitempty"`
}

type PersistedSystemService struct {
	Enabled *bool `json:"enabled,omitempty"`
	Exposed *bool `json:"exposed,omitempty"`
}

type PersistedIntercom struct {
	Info   PersistedIntercomInfo   `json:"info"`
	Auth   AuthState               `json:"auth"`
	Config PersistedIntercomConfig `json:"config"`
}

type PersistedIntercomInfo struct {
	Model string `json:"model,omitempty"`
}

type PersistedIntercomConfig struct {
	Entrypoints []entrypoint.Model       `json:"entrypoints"`
	Audio       PersistedIntercomAudio   `json:"audio"`
	Voicemail   PersistedIntercomMailbox `json:"voicemail"`
}

type PersistedIntercomAudio struct {
	Enabled *bool `json:"enabled,omitempty"`
	Exposed *bool `json:"exposed,omitempty"`
}

type PersistedIntercomMailbox struct {
	MessagesDir string `json:"messages_dir,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
	Exposed     *bool  `json:"exposed,omitempty"`
}

func Default() Config {
	return Config{
		SchemaVersion:             SchemaVersion,
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
		DeviceWiFiRSSI:            nil,
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
		MediaRTSPPathMain:         "doorbell",
		MediaRTPAudioPort:         5000,
		MediaRTPVideoPort:         5007,
		VoicemailMessagesDir:      "/home/bticino/cfg/extra/47/messages",
		SystemRebootEnabled:       true,
		SystemServices: map[string]SystemServiceConfig{
			"dropbear": {
				Enabled: true,
				Exposed: true,
			},
		},
		MuteEnabled:           true,
		ExposeMuteControl:     true,
		VoicemailEnabled:      true,
		ExposeVoicemailToggle: true,
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

	cfg.DeviceModel = strings.TrimSpace(persisted.Intercom.Info.Model)
	cfg.Auth = persisted.Intercom.Auth
	cfg.ClaimCode = strings.TrimSpace(cfg.Auth.ClaimCode)

	cfg.SystemRebootEnabled = boolFromPtr(persisted.System.Control.Reboot.Enabled, cfg.SystemRebootEnabled)
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

	if len(persisted.Intercom.Config.Entrypoints) > 0 {
		cfg.Entrypoints = persisted.Intercom.Config.Entrypoints
	}
	cfg.MuteEnabled = boolFromPtr(persisted.Intercom.Config.Audio.Enabled, cfg.MuteEnabled)
	cfg.ExposeMuteControl = boolFromPtr(persisted.Intercom.Config.Audio.Exposed, cfg.ExposeMuteControl)
	if strings.TrimSpace(persisted.Intercom.Config.Voicemail.MessagesDir) != "" {
		cfg.VoicemailMessagesDir = strings.TrimSpace(persisted.Intercom.Config.Voicemail.MessagesDir)
	}
	cfg.VoicemailEnabled = boolFromPtr(persisted.Intercom.Config.Voicemail.Enabled, cfg.VoicemailEnabled)
	cfg.ExposeVoicemailToggle = boolFromPtr(persisted.Intercom.Config.Voicemail.Exposed, cfg.ExposeVoicemailToggle)

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
			},
			Services: persistedServices,
			Future:   map[string]any{},
		},
		Intercom: PersistedIntercom{
			Info: PersistedIntercomInfo{
				Model: strings.TrimSpace(cfg.DeviceModel),
			},
			Auth: configAuthState(cfg),
			Config: PersistedIntercomConfig{
				Entrypoints: cfg.Entrypoints,
				Audio: PersistedIntercomAudio{
					Enabled: boolPtr(cfg.MuteEnabled),
					Exposed: boolPtr(cfg.ExposeMuteControl),
				},
				Voicemail: PersistedIntercomMailbox{
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

func (c *Config) normalize() {
	if c.SchemaVersion <= 0 {
		c.SchemaVersion = SchemaVersion
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
	c.Auth.RefreshToken = strings.TrimSpace(c.Auth.RefreshToken)
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
	if strings.TrimSpace(c.MediaSIPTransport) == "" {
		c.MediaSIPTransport = "tcp"
	}
	if strings.TrimSpace(c.MediaSIPListen) == "" {
		c.MediaSIPListen = "0.0.0.0:5070"
	}
	if strings.TrimSpace(c.MediaSIPFrom) == "" {
		c.MediaSIPFrom = "webrtc@127.0.0.1"
	}
	if strings.TrimSpace(c.MediaSIPTo) == "" {
		c.MediaSIPTo = "c300x@127.0.0.1"
	}
	if strings.TrimSpace(c.MediaRTSPAddress) == "" {
		c.MediaRTSPAddress = ":8554"
	}
	if strings.TrimSpace(c.MediaRTSPPathMain) == "" {
		c.MediaRTSPPathMain = "doorbell"
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

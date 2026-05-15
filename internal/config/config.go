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

type Config struct {
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
	Auth                      AuthState          `json:"auth"`
	Entrypoints               []entrypoint.Model `json:"entrypoints"`
	OpenWebNetCommandPassword string             `json:"openwebnet_command_password,omitempty"`
	VoicemailMessagesDir      string             `json:"voicemail_messages_dir,omitempty"`
	DeviceModel               string             `json:"device_model,omitempty"`
	DeviceFirmware            string             `json:"device_firmware,omitempty"`
	DeviceHardware            string             `json:"device_hardware,omitempty"`
	DeviceKernel              string             `json:"device_kernel,omitempty"`
	DeviceDistribution        string             `json:"device_distribution,omitempty"`
	DeviceIP                  string             `json:"device_ip,omitempty"`
	DeviceNetmask             string             `json:"device_netmask,omitempty"`
	DeviceMAC                 string             `json:"device_mac,omitempty"`
}

func Default() Config {
	return Config{
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

	cfg.Auth = persisted.Auth
	cfg.ClaimCode = strings.TrimSpace(persisted.Auth.ClaimCode)
	cfg.OpenWebNetCommandPassword = persisted.OpenWebNetCommandPassword
	if strings.TrimSpace(persisted.DeviceModel) != "" {
		cfg.DeviceModel = persisted.DeviceModel
	}
	if strings.TrimSpace(persisted.DeviceFirmware) != "" {
		cfg.DeviceFirmware = persisted.DeviceFirmware
	}
	if strings.TrimSpace(persisted.DeviceHardware) != "" {
		cfg.DeviceHardware = persisted.DeviceHardware
	}
	if strings.TrimSpace(persisted.DeviceKernel) != "" {
		cfg.DeviceKernel = persisted.DeviceKernel
	}
	if strings.TrimSpace(persisted.DeviceDistribution) != "" {
		cfg.DeviceDistribution = persisted.DeviceDistribution
	}
	if strings.TrimSpace(persisted.DeviceIP) != "" {
		cfg.DeviceIP = persisted.DeviceIP
	}
	if strings.TrimSpace(persisted.DeviceNetmask) != "" {
		cfg.DeviceNetmask = persisted.DeviceNetmask
	}
	if strings.TrimSpace(persisted.DeviceMAC) != "" {
		cfg.DeviceMAC = persisted.DeviceMAC
	}
	if strings.TrimSpace(persisted.VoicemailMessagesDir) != "" {
		cfg.VoicemailMessagesDir = persisted.VoicemailMessagesDir
	}
	if len(persisted.Entrypoints) > 0 {
		cfg.Entrypoints = persisted.Entrypoints
	}

	cfg.normalize()
	return cfg, nil
}

func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	cfg.normalize()

	persisted := PersistedConfig{
		Auth:                      configAuthState(cfg),
		Entrypoints:               cfg.Entrypoints,
		OpenWebNetCommandPassword: strings.TrimSpace(cfg.OpenWebNetCommandPassword),
		VoicemailMessagesDir:      strings.TrimSpace(cfg.VoicemailMessagesDir),
		DeviceModel:               strings.TrimSpace(cfg.DeviceModel),
		DeviceFirmware:            strings.TrimSpace(cfg.DeviceFirmware),
		DeviceHardware:            strings.TrimSpace(cfg.DeviceHardware),
		DeviceKernel:              strings.TrimSpace(cfg.DeviceKernel),
		DeviceDistribution:        strings.TrimSpace(cfg.DeviceDistribution),
		DeviceIP:                  strings.TrimSpace(cfg.DeviceIP),
		DeviceNetmask:             strings.TrimSpace(cfg.DeviceNetmask),
		DeviceMAC:                 strings.TrimSpace(cfg.DeviceMAC),
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
}

func configAuthState(cfg Config) AuthState {
	auth := cfg.Auth
	auth.ClaimCode = strings.TrimSpace(auth.ClaimCode)
	if auth.ClaimCode == "" {
		auth.ClaimCode = strings.TrimSpace(cfg.ClaimCode)
	}
	return auth
}

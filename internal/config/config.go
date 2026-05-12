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
	ListenAddr            string             `json:"listen_addr"`
	DataDir               string             `json:"data_dir"`
	ClaimCode             string             `json:"claim_code"`
	DeviceName            string             `json:"device_name"`
	DeviceModel           string             `json:"device_model"`
	DeviceFirmware        string             `json:"device_firmware"`
	DeviceIP              string             `json:"device_ip"`
	DeviceMAC             string             `json:"device_mac"`
	DeviceWiFiRSSI        *int               `json:"device_wifi_rssi,omitempty"`
	MDNSEnabled           bool               `json:"mdns_enabled"`
	MDNSServiceType       string             `json:"mdns_service_type"`
	OpenWebNetEnabled     bool               `json:"openwebnet_enabled"`
	OpenWebNetGroup       string             `json:"openwebnet_group"`
	OpenWebNetListenPort  int                `json:"openwebnet_listen_port"`
	OpenWebNetReadBuffer  int                `json:"openwebnet_read_buffer"`
	OpenWebNetCommandHost string             `json:"openwebnet_command_host"`
	OpenWebNetCommandPort int                `json:"openwebnet_command_port"`
	OpenWebNetCommandSec  int                `json:"openwebnet_command_timeout_sec"`
	MediaSIPEnabled       bool               `json:"media_sip_enabled"`
	MediaSIPTransport     string             `json:"media_sip_transport"`
	MediaSIPListen        string             `json:"media_sip_listen"`
	MediaSIPFrom          string             `json:"media_sip_from"`
	MediaSIPTo            string             `json:"media_sip_to"`
	MediaSIPDomain        string             `json:"media_sip_domain"`
	MediaSIPAuthUser      string             `json:"media_sip_auth_user"`
	MediaSIPAuthPass      string             `json:"media_sip_auth_pass"`
	MediaRTSPEnabled      bool               `json:"media_rtsp_enabled"`
	MediaRTSPAddress      string             `json:"media_rtsp_address"`
	MediaRTSPPathMain     string             `json:"media_rtsp_path_main"`
	MediaRTPAudioPort     int                `json:"media_rtp_audio_port"`
	MediaRTPVideoPort     int                `json:"media_rtp_video_port"`
	Auth                  AuthState          `json:"auth"`
	Entrypoints           []entrypoint.Model `json:"entrypoints"`
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

func Default() Config {
	return Config{
		ListenAddr:            "0.0.0.0:8080",
		DataDir:               "/home/bticino/cfg/extra/companion",
		ClaimCode:             "",
		DeviceName:            "BTicino Companion",
		DeviceModel:           "unknown",
		DeviceFirmware:        "unknown",
		DeviceIP:              "",
		DeviceMAC:             "",
		DeviceWiFiRSSI:        nil,
		MDNSEnabled:           true,
		MDNSServiceType:       "_bticomp._tcp",
		OpenWebNetEnabled:     true,
		OpenWebNetGroup:       "239.255.76.67",
		OpenWebNetListenPort:  7667,
		OpenWebNetReadBuffer:  65535,
		OpenWebNetCommandHost: "127.0.0.1",
		OpenWebNetCommandPort: 20000,
		OpenWebNetCommandSec:  3,
		MediaSIPEnabled:       true,
		MediaSIPTransport:     "tcp",
		MediaSIPListen:        "0.0.0.0:5070",
		MediaSIPFrom:          "webrtc@127.0.0.1",
		MediaSIPTo:            "c300x@127.0.0.1",
		MediaSIPDomain:        "",
		MediaSIPAuthUser:      "",
		MediaSIPAuthPass:      "",
		MediaRTSPEnabled:      true,
		MediaRTSPAddress:      ":8554",
		MediaRTSPPathMain:     "doorbell",
		MediaRTPAudioPort:     5000,
		MediaRTPVideoPort:     5007,
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
		return cfg, nil
	}

	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return Config{}, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	cfg.normalize()
	return cfg, nil
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

func Save(path string, cfg Config) error {
	cfg.normalize()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
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
	c.DeviceIP = strings.TrimSpace(c.DeviceIP)
	c.DeviceMAC = strings.TrimSpace(c.DeviceMAC)
	c.MDNSServiceType = strings.TrimSpace(c.MDNSServiceType)
	c.Auth.DeviceID = strings.TrimSpace(c.Auth.DeviceID)
	c.Auth.ClaimCode = strings.TrimSpace(c.Auth.ClaimCode)
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

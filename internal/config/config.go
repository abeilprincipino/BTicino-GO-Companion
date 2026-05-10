package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"bticino-go-companion/internal/domain/entrypoint"
)

type Config struct {
	ListenAddr           string             `json:"listen_addr"`
	DataDir              string             `json:"data_dir"`
	OpenWebNetEnabled    bool               `json:"openwebnet_enabled"`
	OpenWebNetGroup      string             `json:"openwebnet_group"`
	OpenWebNetListenPort int                `json:"openwebnet_listen_port"`
	OpenWebNetReadBuffer int                `json:"openwebnet_read_buffer"`
	Entrypoints          []entrypoint.Model `json:"entrypoints"`
}

func Default() Config {
	return Config{
		ListenAddr:           "0.0.0.0:8080",
		DataDir:              "/home/bticino/cfg/extra/companion/config",
		OpenWebNetEnabled:    true,
		OpenWebNetGroup:      "239.255.76.67",
		OpenWebNetListenPort: 7667,
		OpenWebNetReadBuffer: 65535,
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
		c.DataDir = "/home/bticino/cfg/extra/companion/config"
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

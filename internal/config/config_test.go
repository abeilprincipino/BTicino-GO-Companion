package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultConfigHasEntrypoint(t *testing.T) {
	cfg := Default()
	if !cfg.MDNSEnabled || cfg.MDNSServiceType != "_bticomp._tcp" {
		t.Fatalf("unexpected mDNS defaults: enabled=%v service=%s", cfg.MDNSEnabled, cfg.MDNSServiceType)
	}
	if cfg.OpenWebNetCommandHost != "127.0.0.1" || cfg.OpenWebNetCommandPort != 20000 || cfg.OpenWebNetCommandSec != 3 {
		t.Fatalf("unexpected openwebnet command defaults: host=%s port=%d timeout=%d", cfg.OpenWebNetCommandHost, cfg.OpenWebNetCommandPort, cfg.OpenWebNetCommandSec)
	}
	if cfg.OpenWebNetCommandPassword != "" {
		t.Fatalf("expected empty openwebnet command password by default, got %q", cfg.OpenWebNetCommandPassword)
	}
	if !cfg.MediaRTSPEnabled || cfg.MediaRTSPAddress != ":8554" || cfg.MediaRTSPPathMain != "doorbell" {
		t.Fatalf("unexpected rtsp defaults: enabled=%v addr=%s main=%s", cfg.MediaRTSPEnabled, cfg.MediaRTSPAddress, cfg.MediaRTSPPathMain)
	}
	if cfg.MediaRTPAudioPort != 5000 || cfg.MediaRTPVideoPort != 5007 {
		t.Fatalf("unexpected rtp defaults: audio=%d video=%d", cfg.MediaRTPAudioPort, cfg.MediaRTPVideoPort)
	}
	if len(cfg.Entrypoints) != 1 {
		t.Fatalf("expected 1 default entrypoint, got %d", len(cfg.Entrypoints))
	}
	ep := cfg.Entrypoints[0]
	if ep.DevAddr != "20" || !ep.HasStream || !ep.HasUnlock || !ep.HasRing {
		t.Fatalf("unexpected default entrypoint: %+v", ep)
	}
}

func TestSaveLoadPersistsOpenWebNetCommandPassword(t *testing.T) {
	tDir := t.TempDir()
	path := filepath.Join(tDir, "config.json")

	cfg := Default()
	cfg.OpenWebNetCommandPassword = "pw123"
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if loaded.OpenWebNetCommandPassword != "pw123" {
		t.Fatalf("expected persisted openwebnet command password, got %q", loaded.OpenWebNetCommandPassword)
	}
}

func TestSaveLoadPersistsDeviceModel(t *testing.T) {
	tDir := t.TempDir()
	path := filepath.Join(tDir, "config.json")

	cfg := Default()
	cfg.DeviceModel = "C100X"
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if loaded.DeviceModel != "C100X" {
		t.Fatalf("expected persisted model C100X, got %q", loaded.DeviceModel)
	}
}

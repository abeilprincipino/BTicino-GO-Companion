package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestSaveLoadDoesNotPersistRuntimeDiagnosticMetadata(t *testing.T) {
	tDir := t.TempDir()
	path := filepath.Join(tDir, "config.json")

	cfg := Default()
	cfg.DeviceFirmware = "9.8.7"
	cfg.DeviceHardware = "3.2.1"
	cfg.DeviceKernel = "6.1.2"
	cfg.DeviceDistribution = "1.2.3"
	cfg.DeviceIP = "192.0.2.172"
	cfg.DeviceNetmask = "255.255.255.0"
	cfg.DeviceMAC = "00:11:22:33:44:55"
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	// Runtime diagnostics are not part of persisted schema config and
	// should not be reloaded from disk.
	if loaded.DeviceFirmware != "unknown" || loaded.DeviceHardware != "unknown" || loaded.DeviceKernel != "unknown" || loaded.DeviceDistribution != "unknown" || loaded.DeviceIP != "" || loaded.DeviceNetmask != "" || loaded.DeviceMAC != "" {
		t.Fatalf("runtime diagnostics should not persist in config.json, got fw=%q hw=%q kernel=%q dist=%q ip=%q netmask=%q mac=%q", loaded.DeviceFirmware, loaded.DeviceHardware, loaded.DeviceKernel, loaded.DeviceDistribution, loaded.DeviceIP, loaded.DeviceNetmask, loaded.DeviceMAC)
	}
}

func TestSaveUsesNestedSystemAndConfigSchema(t *testing.T) {
	tDir := t.TempDir()
	path := filepath.Join(tDir, "config.json")

	cfg := Default()
	cfg.ExposeMuteControl = true
	cfg.SystemRebootEnabled = true
	cfg.SystemServices = map[string]SystemServiceConfig{"dropbear": {Enabled: true, Exposed: true}}
	cfg.ExposeVoicemailToggle = true
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "\"system\"") {
		t.Fatalf("expected nested system schema, got: %s", text)
	}
	if !strings.Contains(text, "\"intercom\"") {
		t.Fatalf("expected nested intercom schema, got: %s", text)
	}
	if !strings.Contains(text, "\"schema_version\": 2") {
		t.Fatalf("expected schema version 2, got: %s", text)
	}
	if !strings.Contains(text, "\"update\"") {
		t.Fatalf("expected system.control.update block, got: %s", text)
	}
}

func TestSaveLoadPersistsSystemUpdateControl(t *testing.T) {
	tDir := t.TempDir()
	path := filepath.Join(tDir, "config.json")

	cfg := Default()
	cfg.SystemUpdateEnabled = true
	cfg.SystemUpdateExposed = true
	cfg.SystemUpdateAllowApply = true
	cfg.SystemUpdateAllowRollback = true
	cfg.UpdateReleaseRepo = "owner/repo"
	cfg.UpdateReleaseAsset = "companion"
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if !loaded.SystemUpdateEnabled || !loaded.SystemUpdateExposed || !loaded.SystemUpdateAllowApply || !loaded.SystemUpdateAllowRollback {
		t.Fatalf("expected persisted update controls, got enabled=%v exposed=%v apply=%v rollback=%v", loaded.SystemUpdateEnabled, loaded.SystemUpdateExposed, loaded.SystemUpdateAllowApply, loaded.SystemUpdateAllowRollback)
	}
	if loaded.UpdateReleaseRepo != "owner/repo" {
		t.Fatalf("expected persisted release repo owner/repo, got %q", loaded.UpdateReleaseRepo)
	}
}

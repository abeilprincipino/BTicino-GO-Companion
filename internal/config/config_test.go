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
	if !cfg.MediaRTSPEnabled || cfg.MediaRTSPAddress != ":8554" {
		t.Fatalf("unexpected rtsp defaults: enabled=%v addr=%s", cfg.MediaRTSPEnabled, cfg.MediaRTSPAddress)
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

func TestResolveDefaultStreamDevAddr(t *testing.T) {
	if got := ResolveDefaultStreamDevAddr("C300X", "20"); got != "20" {
		t.Fatalf("expected C300X stream devaddr fallback 20, got %q", got)
	}
	if got := ResolveDefaultStreamDevAddr("C100X", "20"); got != "20" {
		t.Fatalf("expected C100X stream devaddr fallback 20 when modules file is absent, got %q", got)
	}
	if got := ResolveDefaultStreamDevAddr("", ""); got != "20" {
		t.Fatalf("expected empty stream devaddr fallback 20, got %q", got)
	}
}

func TestDetectC100XStreamDevAddr(t *testing.T) {
	tDir := t.TempDir()
	path := filepath.Join(tDir, "mymodules")
	originalPath := c100xModulesPath
	c100xModulesPath = path
	t.Cleanup(func() { c100xModulesPath = originalPath })

	body := `{
  "modules": [
    {"id": "12", "system": "videodoorentry", "deviceType": "EU", "privateAddress": {"addressValues": [{"value": "20"}] }},
    {"id": "34", "system": "lighting", "deviceType": "EU", "privateAddress": {"addressValues": [{"value": "20"}] }}
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write modules file: %v", err)
	}

	if got := detectC100XStreamDevAddr(); got != "12" {
		t.Fatalf("expected detected C100X stream devaddr 12, got %q", got)
	}
	if got := ResolveDefaultStreamDevAddr("C100X", "20"); got != "12" {
		t.Fatalf("expected C100X stream devaddr 12, got %q", got)
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
	if !strings.Contains(text, "\"companion\"") {
		t.Fatalf("expected nested companion schema, got: %s", text)
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
	cfg.SystemUpdateAllowRollback = true
	cfg.UpdateReleaseRepo = "owner/repo"
	cfg.UpdateReleaseAsset = "companion.tar.gz"
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if !loaded.SystemUpdateEnabled || !loaded.SystemUpdateExposed || !loaded.SystemUpdateAllowRollback {
		t.Fatalf("expected persisted update controls, got enabled=%v exposed=%v rollback=%v", loaded.SystemUpdateEnabled, loaded.SystemUpdateExposed, loaded.SystemUpdateAllowRollback)
	}
	if loaded.UpdateReleaseRepo != "owner/repo" {
		t.Fatalf("expected persisted release repo owner/repo, got %q", loaded.UpdateReleaseRepo)
	}
}

func TestAVEndpointDefaults(t *testing.T) {
	cfg := Default()
	if cfg.MediaAVEndpointEnabled != nil {
		t.Fatal("expected av endpoint tri-state default nil (auto)")
	}
	if cfg.MediaAVEndpointHost != "127.0.0.1" || cfg.MediaAVEndpointPort != 30007 {
		t.Fatalf("unexpected av endpoint defaults: %s:%d", cfg.MediaAVEndpointHost, cfg.MediaAVEndpointPort)
	}
	if cfg.MediaAVHighResVideo {
		t.Fatal("expected low-res video default")
	}
	if cfg.DebugLogEnabled || cfg.DebugLogPath != "/tmp/companion-debug.log" {
		t.Fatalf("unexpected debug log defaults: enabled=%v path=%s", cfg.DebugLogEnabled, cfg.DebugLogPath)
	}
}

func TestAVEndpointEnabledAutoResolution(t *testing.T) {
	cfg := Default()
	cfg.DeviceModel = "C100X"
	if !cfg.AVEndpointEnabled() {
		t.Fatal("expected auto-enabled on C100X")
	}
	cfg.DeviceModel = "c100x"
	if !cfg.AVEndpointEnabled() {
		t.Fatal("expected case-insensitive model match")
	}
	cfg.DeviceModel = "C300X"
	if cfg.AVEndpointEnabled() {
		t.Fatal("expected auto-disabled on C300X")
	}
	off := false
	cfg.DeviceModel = "C100X"
	cfg.MediaAVEndpointEnabled = &off
	if cfg.AVEndpointEnabled() {
		t.Fatal("expected explicit false to win over C100X auto")
	}
	on := true
	cfg.DeviceModel = "C300X"
	cfg.MediaAVEndpointEnabled = &on
	if !cfg.AVEndpointEnabled() {
		t.Fatal("expected explicit true to win over C300X auto")
	}
}

func TestAVEndpointAndDebugPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := Default()
	on := true
	port := 31000
	cfg.MediaAVEndpointEnabled = &on
	cfg.MediaAVEndpointHost = "127.0.0.2"
	cfg.MediaAVEndpointPort = port
	cfg.MediaAVHighResVideo = true
	cfg.DebugLogEnabled = true
	cfg.DebugLogPath = "/tmp/custom.log"
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if loaded.MediaAVEndpointEnabled == nil || !*loaded.MediaAVEndpointEnabled {
		t.Fatal("av endpoint enabled flag lost in round trip")
	}
	if loaded.MediaAVEndpointHost != "127.0.0.2" || loaded.MediaAVEndpointPort != 31000 {
		t.Fatalf("av endpoint host/port lost: %s:%d", loaded.MediaAVEndpointHost, loaded.MediaAVEndpointPort)
	}
	if !loaded.MediaAVHighResVideo {
		t.Fatal("high-res flag lost in round trip")
	}
	if !loaded.DebugLogEnabled || loaded.DebugLogPath != "/tmp/custom.log" {
		t.Fatalf("debug log settings lost: enabled=%v path=%s", loaded.DebugLogEnabled, loaded.DebugLogPath)
	}
}

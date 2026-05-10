package config

import "testing"

func TestDefaultConfigHasEntrypoint(t *testing.T) {
	cfg := Default()
	if cfg.OpenWebNetCommandHost != "127.0.0.1" || cfg.OpenWebNetCommandPort != 20000 || cfg.OpenWebNetCommandSec != 3 {
		t.Fatalf("unexpected openwebnet command defaults: host=%s port=%d timeout=%d", cfg.OpenWebNetCommandHost, cfg.OpenWebNetCommandPort, cfg.OpenWebNetCommandSec)
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

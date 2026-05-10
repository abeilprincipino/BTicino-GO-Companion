package config

import "testing"

func TestDefaultConfigHasEntrypoint(t *testing.T) {
	cfg := Default()
	if cfg.OpenWebNetCommandHost != "127.0.0.1" || cfg.OpenWebNetCommandPort != 20000 || cfg.OpenWebNetCommandSec != 3 {
		t.Fatalf("unexpected openwebnet command defaults: host=%s port=%d timeout=%d", cfg.OpenWebNetCommandHost, cfg.OpenWebNetCommandPort, cfg.OpenWebNetCommandSec)
	}
	if len(cfg.Entrypoints) != 1 {
		t.Fatalf("expected 1 default entrypoint, got %d", len(cfg.Entrypoints))
	}
	ep := cfg.Entrypoints[0]
	if ep.DevAddr != "20" || !ep.HasStream || !ep.HasUnlock || !ep.HasRing {
		t.Fatalf("unexpected default entrypoint: %+v", ep)
	}
}

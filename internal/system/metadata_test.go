package system

import "testing"

func TestParseModel(t *testing.T) {
	if got, ok := parseModel("src/gz uri-zia-0 http://local-server-opkg/C300X/zia"); !ok || got != "C300X" {
		t.Fatalf("expected C300X, got %q ok=%v", got, ok)
	}
	if got, ok := parseModel("src/gz uri-shark-0 http://local-server-opkg/C100X/shark"); !ok || got != "C100X" {
		t.Fatalf("expected C100X, got %q ok=%v", got, ok)
	}
	if got, ok := parseModel("no model"); ok || got != "" {
		t.Fatalf("expected unknown, got %q ok=%v", got, ok)
	}
}

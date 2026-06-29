package sipadapter

import (
	"os"
	"path/filepath"
	"testing"

	"bticino-go-companion/internal/config"
)

func TestResolveSIPConfigFromFlexisipProfile(t *testing.T) {
	dir := t.TempDir()

	domainPath := filepath.Join(dir, "domain-registration.conf")
	if err := os.WriteFile(domainPath, []byte("example.local transport=tcp\n"), 0o644); err != nil {
		t.Fatalf("write domain file: %v", err)
	}

	usersPath := filepath.Join(dir, "users.db.txt")
	if err := os.WriteFile(usersPath, []byte("version:1\nsip:c300x@example.local;md5hash\n"), 0o644); err != nil {
		t.Fatalf("write users file: %v", err)
	}

	originalDomainPaths := flexisipDomainRegistrationPaths
	originalConfigPaths := flexisipConfigPaths
	originalUsersPaths := flexisipUsersDBPaths
	flexisipDomainRegistrationPaths = []string{domainPath}
	flexisipConfigPaths = []string{}
	flexisipUsersDBPaths = []string{usersPath}
	t.Cleanup(func() {
		flexisipDomainRegistrationPaths = originalDomainPaths
		flexisipConfigPaths = originalConfigPaths
		flexisipUsersDBPaths = originalUsersPaths
	})

	cfg := config.Default()
	cfg.MediaSIPFrom = "webrtc@127.0.0.1"
	cfg.MediaSIPDomain = ""
	cfg.MediaSIPAuthUser = ""

	resolved, err := resolveSIPConfig(cfg)
	if err != nil {
		t.Fatalf("resolveSIPConfig failed: %v", err)
	}
	if resolved.MediaSIPDomain != "example.local" {
		t.Fatalf("unexpected domain: %s", resolved.MediaSIPDomain)
	}
	if resolved.MediaSIPFrom != "c300x@127.0.0.1" {
		t.Fatalf("unexpected from: %s", resolved.MediaSIPFrom)
	}
	if resolved.MediaSIPAuthUser != "c300x" {
		t.Fatalf("unexpected auth user: %s", resolved.MediaSIPAuthUser)
	}
}

func TestResolveSIPConfigUsesModelTarget(t *testing.T) {
	originalDomainPaths := flexisipDomainRegistrationPaths
	originalConfigPaths := flexisipConfigPaths
	originalUsersPaths := flexisipUsersDBPaths
	flexisipDomainRegistrationPaths = []string{}
	flexisipConfigPaths = []string{}
	flexisipUsersDBPaths = []string{}
	t.Cleanup(func() {
		flexisipDomainRegistrationPaths = originalDomainPaths
		flexisipConfigPaths = originalConfigPaths
		flexisipUsersDBPaths = originalUsersPaths
	})

	tests := []struct {
		name  string
		model string
		want  string
	}{
		{name: "c300x", model: "C300X", want: "c300x@127.0.0.1"},
		{name: "c100x", model: "C100X", want: "c100x@127.0.0.1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.DeviceModel = tc.model
			cfg.MediaSIPTo = ""

			resolved, err := resolveSIPConfig(cfg)
			if err != nil {
				t.Fatalf("resolveSIPConfig failed: %v", err)
			}
			if resolved.MediaSIPTo != tc.want {
				t.Fatalf("expected target %q, got %q", tc.want, resolved.MediaSIPTo)
			}
		})
	}
}

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

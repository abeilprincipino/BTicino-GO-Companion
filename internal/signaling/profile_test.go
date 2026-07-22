package signaling

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverFlexisipDomainPreservesV2Precedence(t *testing.T) {
	dir := t.TempDir()
	registration := filepath.Join(dir, "domain-registration.conf")
	config := filepath.Join(dir, "flexisip.conf")

	if err := os.WriteFile(registration, []byte("registration.example extra\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(config, []byte("aliases=config.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	withFlexisipPaths(t, []string{registration}, []string{config})

	if got := DiscoverFlexisipDomain(); got != "registration.example" {
		t.Fatalf("DiscoverFlexisipDomain() = %q, want registration.example", got)
	}
}

func TestDiscoverFlexisipDomainConfigKeyOrder(t *testing.T) {
	dir := t.TempDir()

	config := filepath.Join(dir, "flexisip.conf")
	if err := os.WriteFile(config, []byte("# aliases=ignored\nreg-domains=registration.example trailing\nauth-domains=auth.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	withFlexisipPaths(t, nil, []string{config})

	if got := DiscoverFlexisipDomain(); got != "registration.example" {
		t.Fatalf("DiscoverFlexisipDomain() = %q, want registration.example", got)
	}
}

func withFlexisipPaths(t *testing.T, registration, config []string) {
	t.Helper()

	oldRegistration, oldConfig := flexisipDomainRegistrationPaths, flexisipConfigPaths
	flexisipDomainRegistrationPaths, flexisipConfigPaths = registration, config

	t.Cleanup(func() {
		flexisipDomainRegistrationPaths, flexisipConfigPaths = oldRegistration, oldConfig
	})
}

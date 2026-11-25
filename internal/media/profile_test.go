package media

import (
	"bticino-go-companion/internal/config"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSourceConfig(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		entrypoint   config.Entrypoint
		sourceConfig SourceConfig
		wantErr      bool
	}{
		{name: "c300x", model: "C300X", entrypoint: config.Entrypoint{DevAddr: "21"}, sourceConfig: SourceConfig{Model: "C300X", DevAddr: "21", HighResVideo: true, Target: "c300x@127.0.0.1"}},
		{name: "c100x without bt eliot discovery", model: "C100X", wantErr: true},
		{name: "unsupported", model: "unknown", wantErr: true},
		{name: "empty", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sourceConfig, err := ResolveSourceConfig(test.model, test.entrypoint)
			if test.wantErr {
				if !errors.Is(err, ErrUnsupportedModel) && !errors.Is(err, ErrC100XDevAddr) {
					t.Fatalf("ResolveSourceConfig(%q) error = %v", test.model, err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if sourceConfig != test.sourceConfig {
				t.Fatalf("ResolveSourceConfig(%q) = %#v, want %#v", test.model, sourceConfig, test.sourceConfig)
			}
		})
	}
}

func TestResolveSourceConfigDiscoversC100XDevAddr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mymodules")
	if err := os.WriteFile(path, []byte(`{"modules":[{"id":"12","system":"videodoorentry","deviceType":"EU","privateAddress":{"addressValues":[{"value":"20"}]}},{"id":"34","system":"lighting","deviceType":"EU","privateAddress":{"addressValues":[{"value":"20"}]}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	originalPath := c100xModulesPath
	c100xModulesPath = path

	t.Cleanup(func() { c100xModulesPath = originalPath })

	sourceConfig, err := ResolveSourceConfig("C100X", config.Entrypoint{DevAddr: "21"})
	if err != nil {
		t.Fatal(err)
	}

	if sourceConfig.DevAddr != "12" {
		t.Fatalf("DEVADDR = %q, want 12", sourceConfig.DevAddr)
	}
}

func TestResolveSourceConfigPrefersC100XEntrypointExternalUnit(t *testing.T) {
	// Two external units, as on an installation whose camera is at 21 while
	// the unit at 20 does not stream, plus a lock sharing address 21.
	const modules = `{"modules":[` +
		`{"id":"iu","system":"videodoorentry","deviceType":"IU","privateAddress":{"addressValues":[{"value":"110"},{"value":"20"}]}},` +
		`{"id":"eu-20","system":"videodoorentry","deviceType":"EU","privateAddress":{"addressValues":[{"value":"20"}]}},` +
		`{"id":"lock-21","system":"automation","deviceType":"Lock","privateAddress":{"addressValues":[{"value":"21"}]}},` +
		`{"id":"eu-21","system":"videodoorentry","deviceType":"EU","privateAddress":{"addressValues":[{"value":"21"}]}},` +
		`{"id":"eu-dup-a","system":"videodoorentry","deviceType":"EU","privateAddress":{"addressValues":[{"value":"30"}]}},` +
		`{"id":"eu-dup-b","system":"videodoorentry","deviceType":"EU","privateAddress":{"addressValues":[{"value":"30"}]}}` +
		`]}`

	path := filepath.Join(t.TempDir(), "mymodules")
	if err := os.WriteFile(path, []byte(modules), 0o600); err != nil {
		t.Fatal(err)
	}

	originalPath := c100xModulesPath
	c100xModulesPath = path

	t.Cleanup(func() { c100xModulesPath = originalPath })

	tests := []struct {
		name    string
		devAddr string
		want    string
	}{
		{name: "entrypoint external unit", devAddr: "21", want: "eu-21"},
		{name: "entrypoint external unit with spaces", devAddr: " 21 ", want: "eu-21"},
		{name: "default external unit", devAddr: "20", want: "eu-20"},
		{name: "no unit at the address falls back to 20", devAddr: "213", want: "eu-20"},
		{name: "ambiguous address falls back to 20", devAddr: "30", want: "eu-20"},
		{name: "empty address falls back to 20", devAddr: "", want: "eu-20"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sourceConfig, err := ResolveSourceConfig("C100X", config.Entrypoint{DevAddr: test.devAddr})
			if err != nil {
				t.Fatal(err)
			}

			if sourceConfig.DevAddr != test.want {
				t.Fatalf("DEVADDR = %q, want %q", sourceConfig.DevAddr, test.want)
			}
		})
	}
}

func TestResolveSourceConfigC100XWithoutAnyMatchingExternalUnit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mymodules")
	if err := os.WriteFile(path, []byte(`{"modules":[{"id":"eu-21","system":"videodoorentry","deviceType":"EU","privateAddress":{"addressValues":[{"value":"21"}]}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	originalPath := c100xModulesPath
	c100xModulesPath = path

	t.Cleanup(func() { c100xModulesPath = originalPath })

	if _, err := ResolveSourceConfig("C100X", config.Entrypoint{DevAddr: "213"}); !errors.Is(err, ErrC100XDevAddr) {
		t.Fatalf("error = %v, want %v", err, ErrC100XDevAddr)
	}
}

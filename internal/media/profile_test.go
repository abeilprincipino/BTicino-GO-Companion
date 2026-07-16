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

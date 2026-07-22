package media

import (
	"bticino-go-companion/internal/config"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

var (
	ErrUnsupportedModel = errors.New("media: unsupported intercom model")
	ErrC100XDevAddr     = errors.New("media: c100x devaddr discovery failed")
)

// SourceConfig contains all model-specific settings for a media source.
type SourceConfig struct {
	Model        string
	DevAddr      string
	HighResVideo bool
	Target       string
}

// ResolveSourceConfig returns the only supported configuration for a model.
// Unknown models must never inherit C300X behavior.
func ResolveSourceConfig(model string, entrypoint config.Entrypoint) (SourceConfig, error) {
	switch strings.TrimSpace(strings.ToUpper(model)) {
	case "C300X":
		return SourceConfig{Model: "C300X", DevAddr: strings.TrimSpace(entrypoint.DevAddr), HighResVideo: true, Target: "c300x@127.0.0.1"}, nil
	case "C100X":
		devAddr := detectC100XStreamDevAddr()
		if devAddr == "" {
			return SourceConfig{}, ErrC100XDevAddr
		}

		return SourceConfig{Model: "C100X", DevAddr: devAddr, HighResVideo: false, Target: "c100x@127.0.0.1"}, nil
	default:
		return SourceConfig{}, ErrUnsupportedModel
	}
}

var c100xModulesPath = "/home/bticino/cfg/extra/.bt_eliot/mymodules"

type c100xModule struct {
	ID             string `json:"id"`
	System         string `json:"system"`
	DeviceType     string `json:"deviceType"`
	PrivateAddress struct {
		AddressValues []struct {
			Value string `json:"value"`
		} `json:"addressValues"`
	} `json:"privateAddress"`
}

type c100xModulesFile struct {
	Modules []c100xModule `json:"modules"`
}

func detectC100XStreamDevAddr() string {
	body, err := os.ReadFile(c100xModulesPath)
	if err != nil {
		return ""
	}

	var modules c100xModulesFile
	if err := json.Unmarshal(body, &modules); err != nil {
		return ""
	}

	var matches []string

	for _, module := range modules.Modules {
		if !strings.EqualFold(module.System, "videodoorentry") || !strings.EqualFold(module.DeviceType, "EU") {
			continue
		}

		for _, address := range module.PrivateAddress.AddressValues {
			if strings.TrimSpace(address.Value) == "20" {
				if id := strings.TrimSpace(module.ID); id != "" {
					matches = append(matches, id)
				}

				break
			}
		}
	}

	if len(matches) == 1 {
		return matches[0]
	}

	return ""
}

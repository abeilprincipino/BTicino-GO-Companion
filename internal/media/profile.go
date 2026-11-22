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
		devAddr := detectC100XStreamDevAddr(entrypoint.DevAddr)
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

// c100xDefaultStreamAddress is the external unit a C100X stream used before
// the entrypoint's own address was looked up, and still the fallback.
const c100xDefaultStreamAddress = config.DefaultDoorbellDevAddr

// detectC100XStreamDevAddr returns the bt_eliot module ID the C100X expects in
// a=DEVADDR to stream the external unit at entrypointAddress. The unit at that
// address wins: an installation whose camera is not at 20 has its picture
// there, and the unit at 20 may not exist or answer 481. When the address
// matches no unit, or more than one, the unit at 20 is used as before, so an
// installation that streams today keeps the same camera.
func detectC100XStreamDevAddr(entrypointAddress string) string {
	body, err := os.ReadFile(c100xModulesPath)
	if err != nil {
		return ""
	}

	var modules c100xModulesFile
	if err := json.Unmarshal(body, &modules); err != nil {
		return ""
	}

	if id := c100xExternalUnitID(modules.Modules, entrypointAddress); id != "" {
		return id
	}

	return c100xExternalUnitID(modules.Modules, c100xDefaultStreamAddress)
}

// c100xExternalUnitID returns the ID of the only video door entry external
// unit at address, or "" when there is none or the address is ambiguous.
func c100xExternalUnitID(modules []c100xModule, address string) string {
	address = strings.TrimSpace(address)
	if address == "" {
		return ""
	}

	var matches []string

	for _, module := range modules {
		if !strings.EqualFold(module.System, "videodoorentry") || !strings.EqualFold(module.DeviceType, "EU") {
			continue
		}

		for _, value := range module.PrivateAddress.AddressValues {
			if strings.TrimSpace(value.Value) == address {
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

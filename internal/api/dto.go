package api

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/diagnostics"
	"bticino-go-companion/internal/system"
)

// StateDTO is the complete, versioned state contract sent to API clients.
// Keep configuration and device-internal addresses out of this type.
type StateDTO struct {
	core.State
	Device        DeviceDTO            `json:"device"`
	Entrypoints   []EntrypointDTO      `json:"entrypoints"`
	Diagnostics   diagnostics.Snapshot `json:"diagnostics"`
	SystemControl SystemControlDTO     `json:"system_control"`
}

type DeviceDTO struct {
	Model    string `json:"model"`
	Firmware string `json:"firmware,omitempty"`
	Hardware string `json:"hardware,omitempty"`
}

type EntrypointDTO struct {
	ID           string          `json:"id"`
	Label        string          `json:"label"`
	Capabilities CapabilityDTO   `json:"capabilities"`
	Availability AvailabilityDTO `json:"availability"`
}

type CapabilityDTO struct {
	Unlock   bool `json:"unlock"`
	Stream   bool `json:"stream"`
	Snapshot bool `json:"snapshot"`
}

type AvailabilityDTO struct {
	Unlock   bool `json:"unlock"`
	Stream   bool `json:"stream"`
	Snapshot bool `json:"snapshot"`
}

type SystemControlDTO struct {
	RebootEnabled bool                        `json:"reboot_enabled"`
	Services      map[string]SystemServiceDTO `json:"services"`
	Update        *system.UpdateStatus        `json:"update,omitempty"`
}

type SystemServiceDTO struct {
	Enabled bool `json:"enabled"`
	Exposed bool `json:"exposed"`
}

func entrypointDTO(entrypoint config.Entrypoint, unlockAvailable bool) EntrypointDTO {
	return EntrypointDTO{
		ID:    entrypoint.ID,
		Label: entrypoint.Label,
		Capabilities: CapabilityDTO{
			Unlock: entrypoint.Capabilities.Unlock,
			Stream: entrypoint.Capabilities.Stream,
		},
		Availability: AvailabilityDTO{
			Unlock: entrypoint.Capabilities.Unlock && unlockAvailable,
		},
	}
}

package observability

import (
	"time"

	"bticino-go-companion/internal/services/runtime"
)

type Health struct {
	Live       bool           `json:"live"`
	Ready      bool           `json:"ready"`
	Degraded   bool           `json:"degraded"`
	BootTime   time.Time      `json:"boot_time"`
	Timestamp  time.Time      `json:"timestamp"`
	Reasons    []string       `json:"reasons,omitempty"`
	Components map[string]any `json:"components,omitempty"`
}

func New(boot time.Time, status runtime.Snapshot) Health {
	reasons := make([]string, 0, 4)
	degraded := false

	if status.Control.Enabled && !status.Control.Ready {
		reasons = append(reasons, "control_not_ready")
	}
	if status.SIP.Enabled && !status.SIP.Ready {
		reasons = append(reasons, "sip_not_ready")
	}
	if status.OpenWebNet.Enabled && !status.OpenWebNet.Ready {
		reasons = append(reasons, "openwebnet_not_ready")
	}
	if status.SIP.Error != "" || status.OpenWebNet.Error != "" || status.Control.Error != "" {
		degraded = true
	}

	ready := len(reasons) == 0

	return Health{
		Live:      true,
		Ready:     ready,
		Degraded:  degraded,
		BootTime:  boot,
		Timestamp: time.Now().UTC(),
		Reasons:   reasons,
		Components: map[string]any{
			"sip":        status.SIP,
			"openwebnet": status.OpenWebNet,
			"control":    status.Control,
		},
	}
}

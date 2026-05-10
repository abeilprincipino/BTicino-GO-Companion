package event

import "time"

const (
	SourceOpenWebNet = "openwebnet"
	SourceSIP        = "sip"
	SourceAPI        = "api"
	SourceWatchdog   = "watchdog"
	SourceSystem     = "system"
)

type Envelope struct {
	ID           uint64         `json:"id"`
	Type         string         `json:"type"`
	TS           time.Time      `json:"ts"`
	Source       string         `json:"source"`
	EntrypointID string         `json:"entrypoint_id,omitempty"`
	Payload      map[string]any `json:"payload,omitempty"`
	Raw          string         `json:"raw,omitempty"`
}

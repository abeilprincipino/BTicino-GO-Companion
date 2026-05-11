package events

import (
	"fmt"
	"strings"
	"time"

	"bticino-go-companion/internal/domain/entrypoint"
	"bticino-go-companion/internal/domain/event"
	"bticino-go-companion/internal/protocol/openwebnet"
)

type Normalizer struct {
	byDevAddr map[string]string
}

func NewNormalizer(entrypoints []entrypoint.Model) *Normalizer {
	byDevAddr := make(map[string]string, len(entrypoints))
	for _, ep := range entrypoints {
		devAddr := strings.TrimSpace(ep.DevAddr)
		entrypointID := strings.TrimSpace(ep.ID)
		if devAddr == "" || entrypointID == "" {
			continue
		}
		byDevAddr[devAddr] = entrypointID
	}
	return &Normalizer{byDevAddr: byDevAddr}
}

func (n *Normalizer) Normalize(ev event.Envelope) event.Envelope {
	if ev.TS.IsZero() {
		ev.TS = time.Now()
	}
	if strings.TrimSpace(ev.Source) == "" {
		ev.Source = event.SourceSystem
	}
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	if ev.Raw != "" {
		if _, ok := ev.Payload["raw"]; !ok {
			ev.Payload["raw"] = ev.Raw
		}
	}

	if strings.TrimSpace(ev.EntrypointID) != "" {
		return ev
	}

	if isFloorEvent(ev.Type) {
		ev.EntrypointID = "floor"
		return ev
	}

	devAddr := strings.TrimSpace(stringPayload(ev.Payload, "devaddr"))
	if devAddr == "" && strings.TrimSpace(ev.Raw) != "" {
		devAddr = strings.TrimSpace(openwebnetproto.ExtractAddress(ev.Raw))
		if devAddr != "" {
			ev.Payload["devaddr"] = devAddr
		}
	}

	if entrypointID, ok := n.byDevAddr[devAddr]; ok {
		ev.EntrypointID = entrypointID
	}
	return ev
}

func stringPayload(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	raw, ok := payload[key]
	if !ok {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return val
	case fmt.Stringer:
		return val.String()
	default:
		return ""
	}
}

func isFloorEvent(kind string) bool {
	switch strings.TrimSpace(kind) {
	case event.TypeRingFloorStarted, event.TypeRingFloorEnded:
		return true
	default:
		return false
	}
}

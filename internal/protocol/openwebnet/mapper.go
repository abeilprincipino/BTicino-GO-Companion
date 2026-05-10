package openwebnet

import (
	"time"

	"bticino-go-companion/internal/domain/event"
)

type Mapper struct {
	floorRinging bool
}

func NewMapper() *Mapper {
	return &Mapper{}
}

func (m *Mapper) Map(msg Message) []event.Envelope {
	if msg.System != "OPEN" {
		return nil
	}

	now := time.Now().UTC()
	raw := msg.Raw

	newEvent := func(kind string, payload map[string]any) event.Envelope {
		return event.Envelope{
			Type:    kind,
			TS:      now,
			Source:  event.SourceOpenWebNet,
			Raw:     raw,
			Payload: payload,
		}
	}

	switch {
	case IsFloorRingStart(raw):
		if m.floorRinging {
			return nil
		}
		m.floorRinging = true
		return []event.Envelope{newEvent("ring.floor.started", map[string]any{"raw": raw, "entrance": "floor"})}
	case IsRingStart(raw):
		payload := map[string]any{"raw": raw, "entrance": "default"}
		return []event.Envelope{newEvent("ring.started", payload), newEvent("call.incoming", payload)}
	case IsViewRequest(raw):
		return []event.Envelope{newEvent("call.view_requested", map[string]any{"raw": raw, "entrance": "default"})}
	case IsUnlockOpen(raw):
		addr := ExtractAddress(raw)
		return []event.Envelope{newEvent("unlock.pulse.started", map[string]any{"raw": raw, "devaddr": addr})}
	case IsUnlockClose(raw):
		addr := ExtractAddress(raw)
		return []event.Envelope{newEvent("unlock.pulse.ended", map[string]any{"raw": raw, "devaddr": addr})}
	case raw == FrameAudioMuted:
		return []event.Envelope{newEvent("audio.muted", map[string]any{"raw": raw})}
	case raw == FrameAudioUnmuted:
		return []event.Envelope{newEvent("audio.unmuted", map[string]any{"raw": raw})}
	case IsStreamStartVideo(raw):
		return []event.Envelope{newEvent("stream.started", map[string]any{"raw": raw, "channel": "video"})}
	case IsStreamStartAudio(raw):
		return []event.Envelope{newEvent("stream.started", map[string]any{"raw": raw, "channel": "audio"})}
	case IsStreamProbe(raw):
		return nil
	case IsStreamStop(raw):
		events := []event.Envelope{
			newEvent("stream.stopped", map[string]any{"raw": raw}),
			newEvent("ring.ended", map[string]any{"raw": raw}),
			newEvent("call.ended", map[string]any{"raw": raw}),
		}
		if m.floorRinging {
			m.floorRinging = false
			events = append(events, newEvent("ring.floor.ended", map[string]any{"raw": raw, "entrance": "floor"}))
		}
		return events
	default:
		return nil
	}
}

package openwebnetproto

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

	now := time.Now()
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
		return []event.Envelope{newEvent(event.TypeRingFloorStarted, map[string]any{"raw": raw, "entrance": "floor"})}
	case IsRingStart(raw):
		payload := map[string]any{"raw": raw, "entrance": "default", "devaddr": ExtractAddress(raw)}
		return []event.Envelope{newEvent(event.TypeRingStarted, payload), newEvent(event.TypeCallIncoming, payload)}
	case IsViewRequest(raw):
		return []event.Envelope{newEvent(event.TypeCallViewRequested, map[string]any{"raw": raw, "entrance": "default", "devaddr": ExtractAddress(raw)})}
	case IsUnlockOpen(raw):
		addr := ExtractAddress(raw)
		return []event.Envelope{newEvent(event.TypeUnlockPulseStart, map[string]any{"raw": raw, "devaddr": addr})}
	case IsUnlockClose(raw):
		addr := ExtractAddress(raw)
		return []event.Envelope{newEvent(event.TypeUnlockPulseEnd, map[string]any{"raw": raw, "devaddr": addr})}
	case raw == FrameAudioMuted:
		return []event.Envelope{newEvent(event.TypeAudioMuted, map[string]any{"raw": raw})}
	case raw == FrameAudioUnmuted:
		return []event.Envelope{newEvent(event.TypeAudioUnmuted, map[string]any{"raw": raw})}
	case IsStreamStartVideo(raw):
		return []event.Envelope{newEvent(event.TypeStreamStarted, map[string]any{"raw": raw, "channel": "video"})}
	case IsStreamStartAudio(raw):
		return []event.Envelope{newEvent(event.TypeStreamStarted, map[string]any{"raw": raw, "channel": "audio"})}
	case IsStreamProbe(raw):
		return nil
	case IsStreamStop(raw):
		events := []event.Envelope{
			newEvent(event.TypeStreamStopped, map[string]any{"raw": raw}),
			newEvent(event.TypeRingEnded, map[string]any{"raw": raw}),
			newEvent(event.TypeCallEnded, map[string]any{"raw": raw}),
		}
		if m.floorRinging {
			m.floorRinging = false
			events = append(events, newEvent(event.TypeRingFloorEnded, map[string]any{"raw": raw, "entrance": "floor"}))
		}
		return events
	default:
		return nil
	}
}

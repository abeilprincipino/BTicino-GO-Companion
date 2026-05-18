package openwebnetproto

import (
	"strings"
	"time"

	"bticino-go-companion/internal/domain/event"
)

type Mapper struct {
	floorRinging bool
	recentFrames map[string]time.Time
	dedupeWindow time.Duration
}

func NewMapper() *Mapper {
	return &Mapper{
		recentFrames: make(map[string]time.Time),
		dedupeWindow: 300 * time.Millisecond,
	}
}

func (m *Mapper) Map(msg Message) []event.Envelope {
	if !isMappableSystem(msg.System) {
		return nil
	}

	now := time.Now()
	raw := msg.Raw
	if m.isDuplicateRaw(raw, now) {
		return nil
	}

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
	case IsVoicemailStatus(raw):
		enabled, welcomeEnabled, ok := ParseVoicemailStatus(raw)
		if !ok {
			return nil
		}
		kind := event.TypeVoicemailDisabled
		if enabled {
			kind = event.TypeVoicemailEnabled
		}
		return []event.Envelope{
			newEvent(kind, map[string]any{
				"raw":                     raw,
				"welcome_message_enabled": welcomeEnabled,
			}),
		}
	case IsStreamStartVideo(raw):
		return []event.Envelope{newEvent(event.TypeStreamStarted, map[string]any{"raw": raw, "channel": "video"})}
	case IsStreamStartAudio(raw):
		return []event.Envelope{newEvent(event.TypeStreamStarted, map[string]any{"raw": raw, "channel": "audio"})}
	case IsReceiveVideo(raw):
		where, _ := ParseReceiveVideoWhere(raw)
		return []event.Envelope{
			newEvent(event.TypeCallViewRequested, map[string]any{"raw": raw, "entrance": "default", "where": where}),
		}
	case IsStreamProbe(raw):
		return nil
	case IsStreamStop(raw), IsFreeAVResources(raw):
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

func isMappableSystem(system string) bool {
	switch strings.ToLower(strings.TrimSpace(system)) {
	case "open", "aswm":
		return true
	default:
		return false
	}
}

func (m *Mapper) isDuplicateRaw(raw string, now time.Time) bool {
	key := strings.TrimSpace(raw)
	if key == "" {
		return false
	}
	for frame, ts := range m.recentFrames {
		if now.Sub(ts) > m.dedupeWindow {
			delete(m.recentFrames, frame)
		}
	}
	if ts, ok := m.recentFrames[key]; ok && now.Sub(ts) <= m.dedupeWindow {
		m.recentFrames[key] = now
		return true
	}
	m.recentFrames[key] = now
	return false
}

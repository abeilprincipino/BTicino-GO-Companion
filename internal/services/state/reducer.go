package state

import (
	"strings"

	"bticino-go-companion/internal/domain/event"
)

const (
	CallStateIdle    = "idle"
	CallStateRinging = "ringing"
	CallStateActive  = "active"
)

func applyTransition(s *Snapshot, ev event.Envelope) {
	kind := strings.TrimSpace(ev.Type)
	entrypointID := strings.TrimSpace(ev.EntrypointID)
	setEntrypoint := entrypointID != "" && entrypointID != "floor"

	switch kind {
	case event.TypeRingStarted:
		s.Ringing = true
		s.CallState = CallStateRinging
		if setEntrypoint {
			s.ActiveEntrypoint = entrypointID
		}
	case event.TypeRingEnded:
		s.Ringing = false
		if s.StreamActive {
			s.CallState = CallStateActive
			return
		}
		s.CallState = CallStateIdle
		s.ActiveEntrypoint = ""
	case event.TypeRingFloorStarted:
		s.FloorRinging = true
	case event.TypeRingFloorEnded:
		s.FloorRinging = false
	case event.TypeStreamStarted:
		s.StreamActive = true
		s.CallState = CallStateActive
		if setEntrypoint {
			s.ActiveEntrypoint = entrypointID
		}
	case event.TypeStreamStopped:
		s.StreamActive = false
		if s.Ringing {
			s.CallState = CallStateRinging
			return
		}
		s.CallState = CallStateIdle
		s.ActiveEntrypoint = ""
	case event.TypeCallIncoming:
		s.CallState = CallStateRinging
		if setEntrypoint {
			s.ActiveEntrypoint = entrypointID
		}
	case event.TypeCallAnswered:
		s.CallState = CallStateActive
	case event.TypeCallEnded:
		if s.StreamActive {
			s.CallState = CallStateActive
			return
		}
		if s.Ringing {
			s.CallState = CallStateRinging
			return
		}
		s.CallState = CallStateIdle
		s.ActiveEntrypoint = ""
	case event.TypeCallViewRequested:
		if setEntrypoint {
			s.ActiveEntrypoint = entrypointID
		}
	case event.TypeAudioMuted:
		s.AudioMuted = true
	case event.TypeAudioUnmuted:
		s.AudioMuted = false
	case event.TypeVoicemailEnabled:
		s.VoicemailEnabled = true
		if welcomeEnabled, ok := ev.Payload["welcome_message_enabled"].(bool); ok {
			s.VoicemailWelcomeMessageEnabled = welcomeEnabled
		}
	case event.TypeVoicemailDisabled:
		s.VoicemailEnabled = false
		if welcomeEnabled, ok := ev.Payload["welcome_message_enabled"].(bool); ok {
			s.VoicemailWelcomeMessageEnabled = welcomeEnabled
		}
	}
}

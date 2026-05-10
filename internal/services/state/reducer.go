package state

import "bticino-go-companion/internal/domain/event"

const (
	CallStateIdle    = "idle"
	CallStateRinging = "ringing"
	CallStateActive  = "active"
)

func applyTransition(s *Snapshot, kind string) {
	switch kind {
	case event.TypeRingStarted:
		s.Ringing = true
		s.CallState = CallStateRinging
	case event.TypeRingEnded:
		s.Ringing = false
		if s.StreamActive {
			s.CallState = CallStateActive
			return
		}
		s.CallState = CallStateIdle
	case event.TypeRingFloorStarted:
		s.FloorRinging = true
	case event.TypeRingFloorEnded:
		s.FloorRinging = false
	case event.TypeStreamStarted:
		s.StreamActive = true
		s.CallState = CallStateActive
	case event.TypeStreamStopped:
		s.StreamActive = false
		if s.Ringing {
			s.CallState = CallStateRinging
			return
		}
		s.CallState = CallStateIdle
	case event.TypeCallIncoming:
		s.CallState = CallStateRinging
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
	}
}

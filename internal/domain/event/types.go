package event

const (
	TypeEventInvalid      = "event.invalid"
	TypeHeartbeat         = "heartbeat"
	TypeRingStarted       = "ring.started"
	TypeRingEnded         = "ring.ended"
	TypeRingFloorStarted  = "ring.floor.started"
	TypeRingFloorEnded    = "ring.floor.ended"
	TypeCallIncoming      = "call.incoming"
	TypeCallAnswered      = "call.answered"
	TypeCallEnded         = "call.ended"
	TypeCallViewRequested = "call.view_requested"
	TypeStreamStarted     = "stream.started"
	TypeStreamStopped     = "stream.stopped"
	TypeUnlockPulseStart  = "unlock.pulse.started"
	TypeUnlockPulseEnd    = "unlock.pulse.ended"
	TypeUnlockTriggered   = "unlock.triggered"
	TypeAudioMuted        = "audio.muted"
	TypeAudioUnmuted      = "audio.unmuted"
	TypeVoicemailEnabled  = "voicemail.enabled"
	TypeVoicemailDisabled = "voicemail.disabled"
)

var knownTypes = map[string]struct{}{
	TypeEventInvalid:      {},
	TypeHeartbeat:         {},
	TypeRingStarted:       {},
	TypeRingEnded:         {},
	TypeRingFloorStarted:  {},
	TypeRingFloorEnded:    {},
	TypeCallIncoming:      {},
	TypeCallAnswered:      {},
	TypeCallEnded:         {},
	TypeCallViewRequested: {},
	TypeStreamStarted:     {},
	TypeStreamStopped:     {},
	TypeUnlockPulseStart:  {},
	TypeUnlockPulseEnd:    {},
	TypeUnlockTriggered:   {},
	TypeAudioMuted:        {},
	TypeAudioUnmuted:      {},
	TypeVoicemailEnabled:  {},
	TypeVoicemailDisabled: {},
}

func IsKnownType(kind string) bool {
	_, ok := knownTypes[kind]
	return ok
}

package core

type EntrypointID string

type DialogID string

type StreamID string

type EventType string

const (
	EventRingStarted          EventType = "ring.started"
	EventRingCleared          EventType = "ring.cleared"
	EventIncomingCallStarted  EventType = "call.incoming"
	EventIncomingCallEnded    EventType = "call.incoming_ended"
	EventCallAnswered         EventType = "call.answered"
	EventCallDeclined         EventType = "call.declined"
	EventCallHungUp           EventType = "call.hung_up"
	EventPreviewStarted       EventType = "preview.started"
	EventPreviewStopped       EventType = "preview.stopped"
	EventAudioMuted           EventType = "audio.muted"
	EventAudioUnmuted         EventType = "audio.unmuted"
	EventVoicemailEnabled     EventType = "voicemail.enabled"
	EventVoicemailDisabled    EventType = "voicemail.disabled"
	EventVoicemailUnavailable EventType = "voicemail.unavailable"
)

type Event interface {
	Type() EventType
	event()
}

type RingStarted struct {
	EntrypointID EntrypointID
}

func (RingStarted) Type() EventType { return EventRingStarted }
func (RingStarted) event()          {}

type RingCleared struct {
	EntrypointID EntrypointID
}

func (RingCleared) Type() EventType { return EventRingCleared }
func (RingCleared) event()          {}

type IncomingCallStarted struct {
	DialogID     DialogID
	EntrypointID EntrypointID
}

func (IncomingCallStarted) Type() EventType { return EventIncomingCallStarted }
func (IncomingCallStarted) event()          {}

// CallEndReason explains why a pending incoming call stopped without being
// answered on this endpoint.
type CallEndReason string

const (
	CallEndReasonCancelled CallEndReason = "cancelled"
	CallEndReasonTimeout   CallEndReason = "timeout"
	CallEndReasonElsewhere CallEndReason = "elsewhere"
)

type IncomingCallEnded struct {
	DialogID DialogID
	Reason   CallEndReason
}

func (IncomingCallEnded) Type() EventType { return EventIncomingCallEnded }
func (IncomingCallEnded) event()          {}

type CallAnswered struct {
	DialogID DialogID
}

func (CallAnswered) Type() EventType { return EventCallAnswered }
func (CallAnswered) event()          {}

type CallDeclined struct {
	DialogID DialogID
}

func (CallDeclined) Type() EventType { return EventCallDeclined }
func (CallDeclined) event()          {}

type CallHungUp struct {
	DialogID DialogID
}

func (CallHungUp) Type() EventType { return EventCallHungUp }
func (CallHungUp) event()          {}

type PreviewStarted struct {
	StreamID     StreamID
	EntrypointID EntrypointID
}

func (PreviewStarted) Type() EventType { return EventPreviewStarted }
func (PreviewStarted) event()          {}

type PreviewStopped struct {
	StreamID StreamID
}

func (PreviewStopped) Type() EventType { return EventPreviewStopped }
func (PreviewStopped) event()          {}

type AudioMuted struct{}

func (AudioMuted) Type() EventType { return EventAudioMuted }
func (AudioMuted) event()          {}

type AudioUnmuted struct{}

func (AudioUnmuted) Type() EventType { return EventAudioUnmuted }
func (AudioUnmuted) event()          {}

type VoicemailEnabled struct{}

func (VoicemailEnabled) Type() EventType { return EventVoicemailEnabled }
func (VoicemailEnabled) event()          {}

type VoicemailDisabled struct{}

func (VoicemailDisabled) Type() EventType { return EventVoicemailDisabled }
func (VoicemailDisabled) event()          {}

type VoicemailUnavailable struct{}

func (VoicemailUnavailable) Type() EventType { return EventVoicemailUnavailable }
func (VoicemailUnavailable) event()          {}

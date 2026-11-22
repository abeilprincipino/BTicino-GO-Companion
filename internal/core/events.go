package core

import "time"

type EntrypointID string

type DialogID string

type StreamID string

type EventType string

const (
	EventRingStarted          EventType = "ring.started"
	EventRingCleared          EventType = "ring.cleared"
	EventRingRetargeted       EventType = "ring.retargeted"
	EventRingIdentified       EventType = "ring.identified"
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
	EventIntercomUnavailable  EventType = "intercom.unavailable"
	EventIntercomAvailable    EventType = "intercom.available"
)

type Event interface {
	Type() EventType
	event()
}

type RingStarted struct {
	EntrypointID EntrypointID
	// FrameAddress is the external unit address the ring frame carried, which
	// may not be the unit that rang; see RingIdentified.
	FrameAddress string
}

func (RingStarted) Type() EventType { return EventRingStarted }
func (RingStarted) event()          {}

type RingCleared struct {
	EntrypointID EntrypointID
}

func (RingCleared) Type() EventType { return EventRingCleared }
func (RingCleared) event()          {}

// RingRetargeted moves the ring in progress, and the call, preview and stream
// that belong to it, from one entrypoint to another. A C100X may send a ring
// frame with one external unit's address and name the unit whose camera rings
// in a later frame, so the first attribution can be wrong.
type RingRetargeted struct {
	From EntrypointID
	To   EntrypointID
}

func (RingRetargeted) Type() EventType { return EventRingRetargeted }
func (RingRetargeted) event()          {}

// RingIdentified reports the frame naming the external unit that rang, the
// first one of the ring in progress. It changes no state: a RingRetargeted sent
// before it moves the ring when Provisional was wrong. EntrypointID is empty
// when the unit is not a configured doorbell.
type RingIdentified struct {
	EntrypointID EntrypointID
	// Provisional is the entrypoint the ring frame gave the ring to.
	Provisional EntrypointID
	// FrameAddress is the address that ring frame carried.
	FrameAddress string
	// Delay is how long after the ring frame the unit was named.
	Delay time.Duration
}

func (RingIdentified) Type() EventType { return EventRingIdentified }
func (RingIdentified) event()          {}

// Corrected reports whether the ring frame had given the ring to another
// doorbell than the one that rang.
func (e RingIdentified) Corrected() bool {
	return e.EntrypointID != "" && e.EntrypointID != e.Provisional
}

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

// IntercomUnavailable reports that the intercom firmware does not answer:
// no unlock, stream or ring works until IntercomAvailable. Reason names why,
// see IntercomUnavailableReason.
type IntercomUnavailable struct {
	Reason IntercomUnavailableReason
}

func (IntercomUnavailable) Type() EventType { return EventIntercomUnavailable }
func (IntercomUnavailable) event()          {}

// IntercomAvailable reports that the intercom firmware answers again.
type IntercomAvailable struct{}

func (IntercomAvailable) Type() EventType { return EventIntercomAvailable }
func (IntercomAvailable) event()          {}

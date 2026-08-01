package core

import (
	"errors"
	"fmt"
	"sync"
)

var ErrInvalidTransition = errors.New("invalid state transition")

type CallState string

const (
	CallStateIdle    CallState = "idle"
	CallStateRinging CallState = "ringing"
	CallStateActive  CallState = "active"
	CallStatePreview CallState = "preview"
)

type PhysicalRing struct {
	EntrypointID EntrypointID `json:"entrypoint_id"`
}

type IncomingCall struct {
	DialogID     DialogID     `json:"dialog_id"`
	EntrypointID EntrypointID `json:"entrypoint_id"`
}

type IncomingCallEnd struct {
	DialogID DialogID      `json:"dialog_id"`
	Reason   CallEndReason `json:"reason"`
}

type ActiveCall struct {
	DialogID     DialogID     `json:"dialog_id"`
	EntrypointID EntrypointID `json:"entrypoint_id"`
}

type PreviewStream struct {
	StreamID     StreamID     `json:"stream_id"`
	EntrypointID EntrypointID `json:"entrypoint_id"`
}

type AudioState struct {
	Muted bool `json:"muted"`
}

type VoicemailState struct {
	Enabled bool `json:"enabled"`
}

type State struct {
	Revision            uint64           `json:"revision"`
	CallState           CallState        `json:"call_state"`
	PhysicalRing        *PhysicalRing    `json:"physical_ring,omitempty"`
	IncomingCall        *IncomingCall    `json:"incoming_call,omitempty"`
	LastIncomingCallEnd *IncomingCallEnd `json:"last_incoming_call_end,omitempty"`
	ActiveCall          *ActiveCall      `json:"active_call,omitempty"`
	PreviewStream       *PreviewStream   `json:"preview_stream,omitempty"`
	Audio               AudioState       `json:"audio"`
	Voicemail           *VoicemailState  `json:"voicemail,omitempty"`
}

type Projector struct {
	mu    sync.RWMutex
	state State
}

func NewProjector() *Projector {
	return &Projector{state: State{CallState: CallStateIdle}}
}

func (p *Projector) Snapshot() State {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return cloneState(p.state)
}

func (p *Projector) Apply(event Event) (State, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	next := cloneState(p.state)
	if err := apply(&next, event); err != nil {
		return State{}, err
	}

	next.Revision++
	next.CallState = deriveCallState(next)
	p.state = next

	return cloneState(next), nil
}

func apply(state *State, event Event) error {
	switch event := event.(type) {
	case RingStarted:
		if err := requireEntrypoint(event.EntrypointID); err != nil {
			return err
		}

		state.PhysicalRing = &PhysicalRing{EntrypointID: event.EntrypointID}
	case RingCleared:
		if state.PhysicalRing == nil {
			return nil
		}

		if event.EntrypointID != "" && event.EntrypointID != state.PhysicalRing.EntrypointID {
			return transitionError("cannot clear ring for entrypoint %q", event.EntrypointID)
		}

		state.PhysicalRing = nil
	case IncomingCallStarted:
		if err := requireDialogAndEntrypoint(event.DialogID, event.EntrypointID); err != nil {
			return err
		}

		if state.IncomingCall != nil || state.ActiveCall != nil {
			return transitionError("an incoming or active call already exists")
		}

		state.LastIncomingCallEnd = nil
		state.IncomingCall = &IncomingCall{DialogID: event.DialogID, EntrypointID: event.EntrypointID}
	case IncomingCallEnded:
		if state.IncomingCall == nil {
			return nil
		}

		if event.DialogID != state.IncomingCall.DialogID {
			return transitionError("incoming dialog %q does not exist", event.DialogID)
		}

		state.IncomingCall = nil
		state.LastIncomingCallEnd = &IncomingCallEnd{DialogID: event.DialogID, Reason: event.Reason}
	case CallAnswered:
		incoming, err := matchingIncoming(state, event.DialogID)
		if err != nil {
			return err
		}

		state.LastIncomingCallEnd = nil
		state.ActiveCall = &ActiveCall{DialogID: incoming.DialogID, EntrypointID: incoming.EntrypointID}
		state.IncomingCall = nil
	case CallDeclined:
		if _, err := matchingIncoming(state, event.DialogID); err != nil {
			return err
		}

		state.IncomingCall = nil
	case CallHungUp:
		if state.ActiveCall != nil {
			if event.DialogID != state.ActiveCall.DialogID {
				return transitionError("active dialog %q does not exist", event.DialogID)
			}

			state.ActiveCall = nil

			return nil
		}

		if _, err := matchingIncoming(state, event.DialogID); err != nil {
			return err
		}

		state.IncomingCall = nil
	case PreviewStarted:
		if event.StreamID == "" || event.EntrypointID == "" {
			return transitionError("preview stream and entrypoint are required")
		}

		if state.PreviewStream != nil {
			return transitionError("a preview stream already exists")
		}

		state.PreviewStream = &PreviewStream{StreamID: event.StreamID, EntrypointID: event.EntrypointID}
	case PreviewStopped:
		if state.PreviewStream == nil {
			return transitionError("no preview stream exists")
		}

		if event.StreamID != state.PreviewStream.StreamID {
			return transitionError("preview stream %q does not exist", event.StreamID)
		}

		state.PreviewStream = nil
	case AudioMuted:
		state.Audio.Muted = true
	case AudioUnmuted:
		state.Audio.Muted = false
	case VoicemailEnabled:
		state.Voicemail = &VoicemailState{Enabled: true}
	case VoicemailDisabled:
		state.Voicemail = &VoicemailState{Enabled: false}
	case VoicemailUnavailable:
		state.Voicemail = nil
	default:
		return transitionError("unsupported event %T", event)
	}

	return nil
}

func matchingIncoming(state *State, dialogID DialogID) (*IncomingCall, error) {
	if state.IncomingCall == nil {
		return nil, transitionError("an incoming dialog is required")
	}

	if dialogID != state.IncomingCall.DialogID {
		return nil, transitionError("incoming dialog %q does not exist", dialogID)
	}

	return state.IncomingCall, nil
}

func requireEntrypoint(entrypointID EntrypointID) error {
	if entrypointID == "" {
		return transitionError("entrypoint is required")
	}

	return nil
}

func requireDialogAndEntrypoint(dialogID DialogID, entrypointID EntrypointID) error {
	if dialogID == "" || entrypointID == "" {
		return transitionError("dialog and entrypoint are required")
	}

	return nil
}

func transitionError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidTransition, fmt.Sprintf(format, args...))
}

func deriveCallState(state State) CallState {
	if state.ActiveCall != nil {
		return CallStateActive
	}

	if state.PhysicalRing != nil || state.IncomingCall != nil {
		return CallStateRinging
	}

	if state.PreviewStream != nil {
		return CallStatePreview
	}

	return CallStateIdle
}

func cloneState(state State) State {
	stateCopy := state
	if state.PhysicalRing != nil {
		physicalRing := *state.PhysicalRing
		stateCopy.PhysicalRing = &physicalRing
	}

	if state.IncomingCall != nil {
		incomingCall := *state.IncomingCall
		stateCopy.IncomingCall = &incomingCall
	}

	if state.LastIncomingCallEnd != nil {
		lastEnd := *state.LastIncomingCallEnd
		stateCopy.LastIncomingCallEnd = &lastEnd
	}

	if state.ActiveCall != nil {
		activeCall := *state.ActiveCall
		stateCopy.ActiveCall = &activeCall
	}

	if state.PreviewStream != nil {
		previewStream := *state.PreviewStream
		stateCopy.PreviewStream = &previewStream
	}

	if state.Voicemail != nil {
		voicemail := *state.Voicemail
		stateCopy.Voicemail = &voicemail
	}

	return stateCopy
}

package state

import (
	"testing"

	"bticino-go-companion/internal/domain/event"
)

func TestApplyTransitionStreamStopKeepsRingingState(t *testing.T) {
	s := Snapshot{
		CallState:    CallStateActive,
		StreamActive: true,
		Ringing:      true,
	}

	applyTransition(&s, event.Envelope{Type: event.TypeStreamStopped})

	if s.StreamActive {
		t.Fatal("expected stream inactive")
	}
	if s.CallState != CallStateRinging {
		t.Fatalf("expected ringing state, got %s", s.CallState)
	}
}

func TestApplyTransitionCallAnsweredSetsActive(t *testing.T) {
	s := Snapshot{CallState: CallStateRinging}
	applyTransition(&s, event.Envelope{Type: event.TypeCallAnswered})
	if s.CallState != CallStateActive {
		t.Fatalf("expected active state, got %s", s.CallState)
	}
}

func TestApplyTransitionFloorRingTogglesOnlyFloorFlag(t *testing.T) {
	s := Snapshot{CallState: CallStateIdle}
	applyTransition(&s, event.Envelope{Type: event.TypeRingFloorStarted})
	if !s.FloorRinging {
		t.Fatal("expected floor ringing true")
	}
	if s.CallState != CallStateIdle {
		t.Fatalf("floor ring should not alter call state, got %s", s.CallState)
	}

	applyTransition(&s, event.Envelope{Type: event.TypeRingFloorEnded})
	if s.FloorRinging {
		t.Fatal("expected floor ringing false")
	}
}

func TestApplyTransitionTracksActiveEntrypoint(t *testing.T) {
	s := Snapshot{}
	applyTransition(&s, event.Envelope{Type: event.TypeCallViewRequested, EntrypointID: "gate2"})
	if s.ActiveEntrypoint != "gate2" {
		t.Fatalf("expected gate2, got %q", s.ActiveEntrypoint)
	}

	applyTransition(&s, event.Envelope{Type: event.TypeStreamStarted, EntrypointID: "gate2"})
	if !s.StreamActive {
		t.Fatal("expected stream active")
	}
	if s.ActiveEntrypoint != "gate2" {
		t.Fatalf("expected gate2 while streaming, got %q", s.ActiveEntrypoint)
	}

	applyTransition(&s, event.Envelope{Type: event.TypeStreamStopped})
	if s.ActiveEntrypoint != "" {
		t.Fatalf("expected active entrypoint cleared, got %q", s.ActiveEntrypoint)
	}
}

func TestApplyTransitionTracksAudioMute(t *testing.T) {
	s := Snapshot{}
	applyTransition(&s, event.Envelope{Type: event.TypeAudioMuted})
	if !s.AudioMuted {
		t.Fatal("expected audio muted true")
	}
	applyTransition(&s, event.Envelope{Type: event.TypeAudioUnmuted})
	if s.AudioMuted {
		t.Fatal("expected audio muted false")
	}
}

func TestApplyTransitionTracksVoicemailStatus(t *testing.T) {
	s := Snapshot{}
	applyTransition(&s, event.Envelope{
		Type: event.TypeVoicemailEnabled,
		Payload: map[string]any{
			"welcome_message_enabled": true,
		},
	})
	if !s.VoicemailEnabled {
		t.Fatal("expected voicemail enabled true")
	}
	if !s.VoicemailWelcomeMessageEnabled {
		t.Fatal("expected welcome message enabled true")
	}
	applyTransition(&s, event.Envelope{
		Type: event.TypeVoicemailDisabled,
		Payload: map[string]any{
			"welcome_message_enabled": false,
		},
	})
	if s.VoicemailEnabled {
		t.Fatal("expected voicemail enabled false")
	}
	if s.VoicemailWelcomeMessageEnabled {
		t.Fatal("expected welcome message enabled false")
	}
}

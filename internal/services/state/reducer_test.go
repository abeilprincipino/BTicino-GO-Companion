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

	applyTransition(&s, event.TypeStreamStopped)

	if s.StreamActive {
		t.Fatal("expected stream inactive")
	}
	if s.CallState != CallStateRinging {
		t.Fatalf("expected ringing state, got %s", s.CallState)
	}
}

func TestApplyTransitionCallAnsweredSetsActive(t *testing.T) {
	s := Snapshot{CallState: CallStateRinging}
	applyTransition(&s, event.TypeCallAnswered)
	if s.CallState != CallStateActive {
		t.Fatalf("expected active state, got %s", s.CallState)
	}
}

func TestApplyTransitionFloorRingTogglesOnlyFloorFlag(t *testing.T) {
	s := Snapshot{CallState: CallStateIdle}
	applyTransition(&s, event.TypeRingFloorStarted)
	if !s.FloorRinging {
		t.Fatal("expected floor ringing true")
	}
	if s.CallState != CallStateIdle {
		t.Fatalf("floor ring should not alter call state, got %s", s.CallState)
	}

	applyTransition(&s, event.TypeRingFloorEnded)
	if s.FloorRinging {
		t.Fatal("expected floor ringing false")
	}
}

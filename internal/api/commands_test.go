package api

import (
	"encoding/json"
	"errors"
	"testing"

	"bticino-go-companion/internal/core"
)

func TestProjectorCommands_Answer(t *testing.T) {
	projector := core.NewProjector()
	commands := NewProjectorCommands(projector)
	startIncoming(t, projector, "d1", "main")
	state, err := commands.HandleCommand(nil, Command{Action: "call.answer", Payload: json.RawMessage(`{"dialog_id":"d1"}`)})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	got, ok := state.(core.State)
	if !ok {
		t.Fatalf("state type = %T", state)
	}
	if got.CallState != core.CallStateActive {
		t.Fatalf("state = %q, want active", got.CallState)
	}
}

func TestProjectorCommands_Decline(t *testing.T) {
	projector := core.NewProjector()
	commands := NewProjectorCommands(projector)
	startIncoming(t, projector, "d1", "main")
	if _, err := commands.HandleCommand(nil, Command{Action: "call.decline", Payload: json.RawMessage(`{"dialog_id":"d1"}`)}); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if state := projector.Snapshot(); state.IncomingCall != nil || state.CallState != core.CallStateIdle {
		t.Fatalf("state = %#v", state)
	}
}

func TestProjectorCommands_Hangup(t *testing.T) {
	projector := core.NewProjector()
	commands := NewProjectorCommands(projector)
	startIncoming(t, projector, "d1", "main")
	if _, err := commands.HandleCommand(nil, Command{Action: "call.hangup", Payload: json.RawMessage(`{"dialog_id":"d1"}`)}); err != nil {
		t.Fatalf("hangup: %v", err)
	}
	if state := projector.Snapshot(); state.IncomingCall != nil || state.CallState != core.CallStateIdle {
		t.Fatalf("state = %#v", state)
	}
}

func TestProjectorCommands_MissingDialogID(t *testing.T) {
	projector := core.NewProjector()
	commands := NewProjectorCommands(projector)
	_, err := commands.HandleCommand(nil, Command{Action: "call.answer", Payload: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestProjectorCommands_EntrypointCommandsNotImplemented(t *testing.T) {
	projector := core.NewProjector()
	commands := NewProjectorCommands(projector)
	for _, action := range []string{"entrypoints.main.unlock", "entrypoints.main.stream", "entrypoints.main.snapshot"} {
		_, err := commands.HandleCommand(nil, Command{Action: action})
		if !errors.Is(err, ErrNotImplemented) {
			t.Fatalf("%s: err = %v, want ErrNotImplemented", action, err)
		}
	}
}

func TestProjectorCommands_MediaWebRTCSystemNotImplemented(t *testing.T) {
	projector := core.NewProjector()
	commands := NewProjectorCommands(projector)
	for _, action := range []string{"media.play", "webrtc.offer", "system.reboot"} {
		_, err := commands.HandleCommand(nil, Command{Action: action})
		if !errors.Is(err, ErrNotImplemented) {
			t.Fatalf("%s: err = %v, want ErrNotImplemented", action, err)
		}
	}
}

func TestProjectorCommands_UnknownAction(t *testing.T) {
	projector := core.NewProjector()
	commands := NewProjectorCommands(projector)
	_, err := commands.HandleCommand(nil, Command{Action: "unknown.action"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func startIncoming(t *testing.T, projector *core.Projector, dialogID, entrypoint string) {
	t.Helper()
	if _, err := projector.Apply(core.IncomingCallStarted{DialogID: core.DialogID(dialogID), EntrypointID: core.EntrypointID(entrypoint)}); err != nil {
		t.Fatalf("start incoming: %v", err)
	}
}

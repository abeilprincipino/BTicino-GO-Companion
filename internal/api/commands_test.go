package api

import (
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"bticino-go-companion/internal/system"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProjectorCommands_Answer(t *testing.T) {
	t.Parallel()

	projector := core.NewProjector()
	commands := NewProjectorCommands(projector, nil, nil, nil, nil, nil, nil, nil)
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

//nolint:dupl // intentionally similar tests for decline and hangup actions
func TestProjectorCommands_Decline(t *testing.T) {
	t.Parallel()

	projector := core.NewProjector()
	commands := NewProjectorCommands(projector, nil, nil, nil, nil, nil, nil, nil)
	startIncoming(t, projector, "d1", "main")

	if _, err := commands.HandleCommand(nil, Command{Action: "call.decline", Payload: json.RawMessage(`{"dialog_id":"d1"}`)}); err != nil {
		t.Fatalf("decline: %v", err)
	}

	if state := projector.Snapshot(); state.IncomingCall != nil || state.CallState != core.CallStateIdle {
		t.Fatalf("state = %#v", state)
	}
}

//nolint:dupl // intentionally similar to TestProjectorCommands_Decline
func TestProjectorCommands_Hangup(t *testing.T) {
	t.Parallel()

	projector := core.NewProjector()
	commands := NewProjectorCommands(projector, nil, nil, nil, nil, nil, nil, nil)
	startIncoming(t, projector, "d1", "main")

	if _, err := commands.HandleCommand(nil, Command{Action: "call.hangup", Payload: json.RawMessage(`{"dialog_id":"d1"}`)}); err != nil {
		t.Fatalf("hangup: %v", err)
	}

	if state := projector.Snapshot(); state.IncomingCall != nil || state.CallState != core.CallStateIdle {
		t.Fatalf("state = %#v", state)
	}
}

func TestProjectorCommands_MissingDialogID(t *testing.T) {
	t.Parallel()

	projector := core.NewProjector()
	commands := NewProjectorCommands(projector, nil, nil, nil, nil, nil, nil, nil)

	_, err := commands.HandleCommand(nil, Command{Action: "call.answer", Payload: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestProjectorCommands_EntrypointCommandsUnavailable(t *testing.T) {
	t.Parallel()

	projector := core.NewProjector()
	commands := NewProjectorCommands(projector, nil, nil, nil, nil, nil, nil, nil)

	for _, action := range []string{"entrypoints.main.unlock", "entrypoints.main.stream", "entrypoints.main.snapshot"} {
		_, err := commands.HandleCommand(testRequest(), Command{Action: action})
		if err == nil {
			t.Fatalf("%s: expected error", action)
		}
	}
}

func TestProjectorCommands_MediaWebRTCSystemUnavailable(t *testing.T) {
	t.Parallel()

	projector := core.NewProjector()
	commands := NewProjectorCommands(projector, nil, nil, nil, nil, nil, nil, nil)

	for _, action := range []string{"audio.mute", "webrtc.offer", "system.reboot"} {
		_, err := commands.HandleCommand(testRequest(), Command{Action: action})
		if err == nil {
			t.Fatalf("%s: expected error", action)
		}
	}
}

func TestProjectorCommands_UnknownAction(t *testing.T) {
	t.Parallel()

	projector := core.NewProjector()
	commands := NewProjectorCommands(projector, nil, nil, nil, nil, nil, nil, nil)

	_, err := commands.HandleCommand(nil, Command{Action: "unknown.action"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestProjectorCommands_EntrypointUnlock(t *testing.T) {
	t.Parallel()

	entrypoints := &fakeEntrypointControl{}

	commands := NewProjectorCommands(core.NewProjector(), entrypoints, nil, nil, nil, nil, nil, nil)
	if _, err := commands.HandleCommand(testRequest(), Command{Action: "entrypoints.main.unlock"}); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	if entrypoints.lastEntrypoint != "main" {
		t.Fatalf("entrypoint = %q", entrypoints.lastEntrypoint)
	}
}

func TestProjectorCommands_AudioMute(t *testing.T) {
	t.Parallel()

	audio := &fakeAudioControl{}

	commands := NewProjectorCommands(core.NewProjector(), nil, audio, nil, nil, nil, nil, nil)
	if _, err := commands.HandleCommand(testRequest(), Command{Action: "audio.mute"}); err != nil {
		t.Fatalf("mute: %v", err)
	}

	if !audio.muted {
		t.Fatal("mute was not called")
	}
}

func TestProjectorCommands_VoicemailDisable(t *testing.T) {
	t.Parallel()

	voicemail := &fakeVoicemailControl{}

	commands := NewProjectorCommands(core.NewProjector(), nil, nil, voicemail, nil, nil, nil, nil)
	if _, err := commands.HandleCommand(testRequest(), Command{Action: "voicemail.disable"}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if voicemail.enabled {
		t.Fatal("disable was not called")
	}
}

func TestProjectorCommands_WebRTCOffer(t *testing.T) {
	t.Parallel()

	webrtc := &fakeWebRTCControl{answer: media.SessionDescription{Type: "answer", SDP: "sdp"}}

	commands := NewProjectorCommands(core.NewProjector(), nil, nil, nil, nil, nil, webrtc, nil)

	payload := json.RawMessage(`{"source":{"entrypoint_id":"main","media_kind":2,"ssrc":1,"generation":"g1"},"session_id":"s1","offer":{"type":"offer","sdp":"offer"}}`)

	result, err := commands.HandleCommand(testRequest(), Command{Action: "webrtc.offer", Payload: payload})
	if err != nil {
		t.Fatalf("offer: %v", err)
	}

	answer, ok := result.(media.SessionDescription)
	if !ok || answer.Type != "answer" {
		t.Fatalf("answer = %#v", result)
	}
}

func TestProjectorCommands_WebRTCCandidate(t *testing.T) {
	t.Parallel()

	webrtc := &fakeWebRTCControl{}
	commands := NewProjectorCommands(core.NewProjector(), nil, nil, nil, nil, nil, webrtc, nil)

	payload := json.RawMessage(`{"session_id":"s1","candidate":{"candidate":"c1"}}`)
	if _, err := commands.HandleCommand(testRequest(), Command{Action: "webrtc.candidate", Payload: payload}); err != nil {
		t.Fatalf("candidate: %v", err)
	}

	if webrtc.addCandidate != 1 || webrtc.candidate.Candidate != "c1" {
		t.Fatalf("candidate = %#v", webrtc.candidate)
	}
}

func TestProjectorCommands_WebRTCClose(t *testing.T) {
	t.Parallel()

	webrtc := &fakeWebRTCControl{}
	commands := NewProjectorCommands(core.NewProjector(), nil, nil, nil, nil, nil, webrtc, nil)

	payload := json.RawMessage(`{"session_id":"s1"}`)
	if _, err := commands.HandleCommand(testRequest(), Command{Action: "webrtc.close", Payload: payload}); err != nil {
		t.Fatalf("close: %v", err)
	}

	if webrtc.closed != "s1" {
		t.Fatalf("closed = %q", webrtc.closed)
	}
}

func TestProjectorCommands_SystemReboot(t *testing.T) {
	t.Parallel()

	runtime := &fakeRuntimeControl{}

	commands := NewProjectorCommands(core.NewProjector(), nil, nil, nil, runtime, nil, nil, nil)
	if _, err := commands.HandleCommand(testRequest(), Command{Action: "system.reboot"}); err != nil {
		t.Fatalf("reboot: %v", err)
	}

	if !runtime.rebooted {
		t.Fatal("reboot was not called")
	}
}

func TestProjectorCommands_ServiceRestart(t *testing.T) {
	t.Parallel()

	runtime := &fakeRuntimeControl{}

	commands := NewProjectorCommands(core.NewProjector(), nil, nil, nil, runtime, nil, nil, nil)
	if _, err := commands.HandleCommand(testRequest(), Command{Action: "system.service.companion.restart"}); err != nil {
		t.Fatalf("restart: %v", err)
	}

	if runtime.restarted != "companion" {
		t.Fatalf("restarted = %q", runtime.restarted)
	}
}

func TestProjectorCommands_ServiceStatus(t *testing.T) {
	t.Parallel()

	runtime := &fakeRuntimeControl{status: system.ServiceStatus{Name: "companion", Running: true}}

	commands := NewProjectorCommands(core.NewProjector(), nil, nil, nil, runtime, nil, nil, nil)

	result, err := commands.HandleCommand(testRequest(), Command{Action: "system.service.companion.status"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	status, ok := result.(system.ServiceStatus)
	if !ok || !status.Running {
		t.Fatalf("status = %#v", result)
	}
}

func TestProjectorCommands_UpdateStatus(t *testing.T) {
	t.Parallel()

	update := &fakeUpdateControl{status: system.UpdateStatus{CurrentVersion: "v1", LatestVersion: "v2", UpdateAvailable: true}}
	commands := NewProjectorCommands(core.NewProjector(), nil, nil, nil, nil, update, nil, nil)

	result, err := commands.HandleCommand(testRequest(), Command{Action: "system.update.status"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	status, ok := result.(system.UpdateStatus)
	if !ok || !status.UpdateAvailable {
		t.Fatalf("status = %#v", result)
	}
}

func TestProjectorCommands_UpdateCheck(t *testing.T) {
	t.Parallel()

	update := &fakeUpdateControl{manifest: system.ReleaseManifest{TagName: "v2"}}
	commands := NewProjectorCommands(core.NewProjector(), nil, nil, nil, nil, update, nil, nil)

	result, err := commands.HandleCommand(testRequest(), Command{Action: "system.update.check"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	manifest, ok := result.(system.ReleaseManifest)
	if !ok || manifest.TagName != "v2" {
		t.Fatalf("manifest = %#v", result)
	}
}

func TestProjectorCommands_UpdateApply(t *testing.T) {
	t.Parallel()

	update := &fakeUpdateControl{}

	commands := NewProjectorCommands(core.NewProjector(), nil, nil, nil, nil, update, nil, nil)

	payload := json.RawMessage(`{"asset_name":"companion.tar.gz","plan":{"service":"companion","rotation":{"current_path":"/opt/companion","previous_path":"/opt/companion.previous"},"health_url":"http://127.0.0.1:8080/api/v3/health","health_timeout":60000000000}}`)
	if _, err := commands.HandleCommand(testRequest(), Command{Action: "system.update.apply", Payload: payload}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if update.applied.AssetName != "companion.tar.gz" {
		t.Fatalf("applied = %#v", update.applied)
	}
}

func TestProjectorCommands_UpdateRollback(t *testing.T) {
	t.Parallel()

	update := &fakeUpdateControl{}

	commands := NewProjectorCommands(core.NewProjector(), nil, nil, nil, nil, update, nil, nil)
	if _, err := commands.HandleCommand(testRequest(), Command{Action: "system.update.rollback"}); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if !update.rolledBack {
		t.Fatal("rollback was not called")
	}
}

func TestProjectorCommands_Snapshot(t *testing.T) {
	t.Parallel()

	entrypoints := &fakeEntrypointControl{snapshotImage: []byte("image")}
	commands := NewProjectorCommands(core.NewProjector(), entrypoints, nil, nil, nil, nil, nil, nil)

	result, err := commands.HandleCommand(testRequest(), Command{Action: "entrypoints.main.snapshot"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	data, ok := result.([]byte)
	if !ok || string(data) != "image" {
		t.Fatalf("snapshot = %v", result)
	}
}

func startIncoming(t *testing.T, projector *core.Projector, dialogID, entrypoint string) {
	t.Helper()

	if _, err := projector.Apply(core.IncomingCallStarted{DialogID: core.DialogID(dialogID), EntrypointID: core.EntrypointID(entrypoint)}); err != nil {
		t.Fatalf("start incoming: %v", err)
	}
}

func testRequest() *http.Request {
	return httptest.NewRequestWithContext(context.Background(), "POST", "/", nil)
}

type fakeEntrypointControl struct {
	lastEntrypoint core.EntrypointID
	lastUnlock     int
	lastStream     int
	lastSnapshot   int
	snapshotImage  []byte
}

func (f *fakeEntrypointControl) Unlock(_ context.Context, entrypointID core.EntrypointID) error {
	f.lastEntrypoint = entrypointID
	f.lastUnlock++

	return nil
}

func (f *fakeEntrypointControl) Stream(_ context.Context, entrypointID core.EntrypointID) error {
	f.lastEntrypoint = entrypointID
	f.lastStream++

	return nil
}

func (f *fakeEntrypointControl) Snapshot(_ context.Context, entrypointID core.EntrypointID) ([]byte, error) {
	f.lastEntrypoint = entrypointID

	f.lastSnapshot++
	if f.snapshotImage != nil {
		return f.snapshotImage, nil
	}

	return []byte("snapshot"), nil
}

type fakeAudioControl struct {
	muted   bool
	unmuted bool
}

func (f *fakeAudioControl) Mute(context.Context) error {
	f.muted = true
	return nil
}

func (f *fakeAudioControl) Unmute(context.Context) error {
	f.unmuted = true
	return nil
}

type fakeVoicemailControl struct {
	enabled  bool
	disabled bool
}

func (f *fakeVoicemailControl) Enable(context.Context) error {
	f.enabled = true
	return nil
}

func (f *fakeVoicemailControl) Disable(context.Context) error {
	f.disabled = true
	return nil
}

type fakeWebRTCControl struct {
	answer       media.SessionDescription
	candidate    media.ICECandidate
	closed       media.SessionID
	addCandidate int
	closeCount   int
}

func (f *fakeWebRTCControl) Offer(_ media.Source, _ media.SessionID, _ media.SessionDescription) (media.SessionDescription, error) {
	return f.answer, nil
}

func (f *fakeWebRTCControl) AddCandidate(_ media.SessionID, candidate media.ICECandidate) error {
	f.candidate = candidate
	f.addCandidate++

	return nil
}

func (f *fakeWebRTCControl) Close(sessionID media.SessionID) error {
	f.closed = sessionID
	f.closeCount++

	return nil
}

type fakeRuntimeControl struct {
	rebooted      bool
	restarted     string
	status        system.ServiceStatus
	statusQueried string
}

func (f *fakeRuntimeControl) Reboot(context.Context) error {
	f.rebooted = true
	return nil
}

func (f *fakeRuntimeControl) Restart(_ context.Context, service string) error {
	f.restarted = service
	return nil
}

func (f *fakeRuntimeControl) Status(_ context.Context, service string) (system.ServiceStatus, error) {
	f.statusQueried = service
	return f.status, nil
}

type fakeUpdateControl struct {
	manifest   system.ReleaseManifest
	status     system.UpdateStatus
	applied    system.UpdateRequest
	rolledBack bool
}

func (f *fakeUpdateControl) Check(context.Context) (system.ReleaseManifest, error) {
	return f.manifest, nil
}

func (f *fakeUpdateControl) Status(context.Context) (system.UpdateStatus, error) {
	return f.status, nil
}

func (f *fakeUpdateControl) Apply(_ context.Context, req system.UpdateRequest) error {
	f.applied = req
	return nil
}

func (f *fakeUpdateControl) Rollback(context.Context) error {
	f.rolledBack = true
	return nil
}

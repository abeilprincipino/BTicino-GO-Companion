package api

import (
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"bticino-go-companion/internal/system"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

var ErrNotImplemented = errors.New("not implemented")

type ProjectorCommands struct {
	projector   *core.Projector
	entrypoints EntrypointControl
	audio       AudioControl
	voicemail   VoicemailControl
	runtime     RuntimeControl
	update      UpdateControl
	webrtc      WebRTCControl
	snapshot    SnapshotControl
}

func NewProjectorCommands(
	projector *core.Projector,
	entrypoints EntrypointControl,
	audio AudioControl,
	voicemail VoicemailControl,
	runtime RuntimeControl,
	update UpdateControl,
	webrtc WebRTCControl,
	snapshot SnapshotControl,
) *ProjectorCommands {
	return &ProjectorCommands{
		projector:   projector,
		entrypoints: entrypoints,
		audio:       audio,
		voicemail:   voicemail,
		runtime:     runtime,
		update:      update,
		webrtc:      webrtc,
		snapshot:    snapshot,
	}
}

func (p *ProjectorCommands) HandleCommand(r *http.Request, cmd Command) (any, error) {
	switch {
	case cmd.Action == "call.answer":
		return p.answer(cmd.Payload)
	case cmd.Action == "call.decline":
		return p.decline(cmd.Payload)
	case cmd.Action == "call.hangup":
		return p.hangup(cmd.Payload)
	case isEntrypointAction(cmd.Action, "unlock"):
		return p.entrypointCommand(r.Context(), cmd.Action, func(ctx context.Context, id core.EntrypointID) error { return p.entrypoints.Unlock(ctx, id) })
	case isEntrypointAction(cmd.Action, "stream"):
		return p.entrypointCommand(r.Context(), cmd.Action, func(ctx context.Context, id core.EntrypointID) error { return p.entrypoints.Stream(ctx, id) })
	case isEntrypointAction(cmd.Action, "snapshot"):
		return p.entrypointSnapshot(r.Context(), cmd.Action)
	case cmd.Action == "media.mute", cmd.Action == "audio.mute":
		return nil, p.audioCommand(func(ctx context.Context) error { return p.audio.Mute(ctx) }, r.Context())
	case cmd.Action == "media.unmute", cmd.Action == "audio.unmute":
		return nil, p.audioCommand(func(ctx context.Context) error { return p.audio.Unmute(ctx) }, r.Context())
	case cmd.Action == "voicemail.enable":
		return nil, p.voicemailCommand(func(ctx context.Context) error { return p.voicemail.Enable(ctx) }, r.Context())
	case cmd.Action == "voicemail.disable":
		return nil, p.voicemailCommand(func(ctx context.Context) error { return p.voicemail.Disable(ctx) }, r.Context())
	case cmd.Action == "webrtc.offer":
		return p.webrtcOffer(r.Context(), cmd.Payload)
	case cmd.Action == "webrtc.candidate":
		return nil, p.webrtcCandidate(r.Context(), cmd.Payload)
	case cmd.Action == "webrtc.close":
		return nil, p.webrtcClose(r.Context(), cmd.Payload)
	case cmd.Action == "system.reboot":
		return nil, p.runtimeCommand(func(ctx context.Context) error { return p.runtime.Reboot(ctx) }, r.Context())
	case isServiceAction(cmd.Action, "restart"):
		return p.serviceCommand(r.Context(), cmd.Action, p.runtime.Restart)
	case isServiceAction(cmd.Action, "status"):
		return p.serviceStatusCommand(r.Context(), cmd.Action)
	case cmd.Action == "system.update.status":
		return p.updateCommand(func(ctx context.Context) (any, error) { return p.update.Status(ctx) }, r.Context())
	case cmd.Action == "system.update.check":
		return p.updateCommand(func(ctx context.Context) (any, error) { return p.update.Check(ctx) }, r.Context())
	case cmd.Action == "system.update.apply":
		return nil, p.updateApply(r.Context(), cmd.Payload)
	case cmd.Action == "system.update.rollback":
		return nil, p.updateCommandErr(func(ctx context.Context) error { return p.update.Rollback(ctx) }, r.Context())
	default:
		return nil, errors.New("unsupported action")
	}
}

func (p *ProjectorCommands) answer(payload json.RawMessage) (any, error) {
	dialogID, err := requireDialogID(payload)
	if err != nil {
		return nil, err
	}

	return p.projector.Apply(core.CallAnswered{DialogID: dialogID})
}

func (p *ProjectorCommands) decline(payload json.RawMessage) (any, error) {
	dialogID, err := requireDialogID(payload)
	if err != nil {
		return nil, err
	}

	return p.projector.Apply(core.CallDeclined{DialogID: dialogID})
}

func (p *ProjectorCommands) hangup(payload json.RawMessage) (any, error) {
	dialogID, err := requireDialogID(payload)
	if err != nil {
		return nil, err
	}

	return p.projector.Apply(core.CallHungUp{DialogID: dialogID})
}

func requireDialogID(payload json.RawMessage) (core.DialogID, error) {
	var body struct {
		DialogID string `json:"dialog_id"`
	}

	if len(payload) == 0 {
		return "", errors.New("dialog_id is required")
	}

	if json.Unmarshal(payload, &body) != nil {
		return "", errors.New("invalid payload")
	}

	if body.DialogID == "" {
		return "", errors.New("dialog_id is required")
	}

	return core.DialogID(body.DialogID), nil
}

func isEntrypointAction(action, verb string) bool {
	parts := strings.Split(action, ".")
	return len(parts) == 3 && parts[0] == "entrypoints" && parts[2] == verb
}

func isServiceAction(action, verb string) bool {
	parts := strings.Split(action, ".")
	return len(parts) == 4 && parts[0] == "system" && parts[1] == "service" && parts[3] == verb
}

func (p *ProjectorCommands) entrypointCommand(ctx context.Context, action string, fn func(context.Context, core.EntrypointID) error) (any, error) {
	entrypointID, err := parseEntrypointID(action)
	if err != nil {
		return nil, err
	}

	if p.entrypoints == nil {
		return nil, errors.New("entrypoint control is unavailable")
	}

	return nil, fn(ctx, entrypointID)
}

func (p *ProjectorCommands) entrypointSnapshot(ctx context.Context, action string) (any, error) {
	entrypointID, err := parseEntrypointID(action)
	if err != nil {
		return nil, err
	}

	if p.entrypoints == nil {
		return nil, errors.New("entrypoint control is unavailable")
	}

	return p.entrypoints.Snapshot(ctx, entrypointID)
}

func parseEntrypointID(action string) (core.EntrypointID, error) {
	parts := strings.Split(action, ".")
	if len(parts) != 3 || parts[0] != "entrypoints" {
		return "", errors.New("invalid entrypoint action")
	}

	return core.EntrypointID(parts[1]), nil
}

func (p *ProjectorCommands) audioCommand(fn func(context.Context) error, ctx context.Context) error {
	if p.audio == nil {
		return errors.New("audio control is unavailable")
	}

	return fn(ctx)
}

func (p *ProjectorCommands) voicemailCommand(fn func(context.Context) error, ctx context.Context) error {
	if p.voicemail == nil {
		return errors.New("voicemail control is unavailable")
	}

	return fn(ctx)
}

func (p *ProjectorCommands) webrtcOffer(ctx context.Context, payload json.RawMessage) (any, error) {
	var req struct {
		Source    media.Source             `json:"source"`
		SessionID media.SessionID          `json:"session_id"`
		Offer     media.SessionDescription `json:"offer"`
	}

	if json.Unmarshal(payload, &req) != nil {
		return nil, errors.New("invalid payload")
	}

	if p.webrtc == nil {
		return nil, errors.New("webrtc control is unavailable")
	}

	return p.webrtc.Offer(req.Source, req.SessionID, req.Offer)
}

func (p *ProjectorCommands) webrtcCandidate(ctx context.Context, payload json.RawMessage) error {
	var req struct {
		SessionID media.SessionID    `json:"session_id"`
		Candidate media.ICECandidate `json:"candidate"`
	}

	if json.Unmarshal(payload, &req) != nil {
		return errors.New("invalid payload")
	}

	if p.webrtc == nil {
		return errors.New("webrtc control is unavailable")
	}

	return p.webrtc.AddCandidate(req.SessionID, req.Candidate)
}

func (p *ProjectorCommands) webrtcClose(ctx context.Context, payload json.RawMessage) error {
	var req struct {
		SessionID media.SessionID `json:"session_id"`
	}

	if json.Unmarshal(payload, &req) != nil {
		return errors.New("invalid payload")
	}

	if p.webrtc == nil {
		return errors.New("webrtc control is unavailable")
	}

	return p.webrtc.Close(req.SessionID)
}

func (p *ProjectorCommands) runtimeCommand(fn func(context.Context) error, ctx context.Context) error {
	if p.runtime == nil {
		return errors.New("runtime control is unavailable")
	}

	return fn(ctx)
}

func (p *ProjectorCommands) serviceCommand(ctx context.Context, action string, fn func(context.Context, string) error) (any, error) {
	service, err := parseServiceName(action)
	if err != nil {
		return nil, err
	}

	if p.runtime == nil {
		return nil, errors.New("runtime control is unavailable")
	}

	return nil, fn(ctx, service)
}

func (p *ProjectorCommands) serviceStatusCommand(ctx context.Context, action string) (any, error) {
	service, err := parseServiceName(action)
	if err != nil {
		return nil, err
	}

	if p.runtime == nil {
		return nil, errors.New("runtime control is unavailable")
	}

	return p.runtime.Status(ctx, service)
}

func parseServiceName(action string) (string, error) {
	parts := strings.Split(action, ".")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "service" {
		return "", errors.New("invalid service action")
	}

	return parts[2], nil
}

func (p *ProjectorCommands) updateCommand(fn func(context.Context) (any, error), ctx context.Context) (any, error) {
	if p.update == nil {
		return nil, errors.New("update control is unavailable")
	}

	return fn(ctx)
}

func (p *ProjectorCommands) updateCommandErr(fn func(context.Context) error, ctx context.Context) error {
	if p.update == nil {
		return errors.New("update control is unavailable")
	}

	return fn(ctx)
}

func (p *ProjectorCommands) updateApply(ctx context.Context, payload json.RawMessage) error {
	var req system.UpdateRequest

	if json.Unmarshal(payload, &req) != nil {
		return errors.New("invalid payload")
	}

	if p.update == nil {
		return errors.New("update control is unavailable")
	}

	return p.update.Apply(ctx, req)
}

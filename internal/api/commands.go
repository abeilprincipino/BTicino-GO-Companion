package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"bticino-go-companion/internal/core"
)

var ErrNotImplemented = errors.New("not implemented")

type ProjectorCommands struct {
	projector *core.Projector
}

func NewProjectorCommands(projector *core.Projector) *ProjectorCommands {
	return &ProjectorCommands{projector: projector}
}

func (p *ProjectorCommands) HandleCommand(r *http.Request, cmd Command) (any, error) {
	switch {
	case cmd.Action == "call.answer":
		return p.answer(cmd.Payload)
	case cmd.Action == "call.decline":
		return p.decline(cmd.Payload)
	case cmd.Action == "call.hangup":
		return p.hangup(cmd.Payload)
	case isEntrypointAction(cmd.Action, "unlock"), isEntrypointAction(cmd.Action, "stream"), isEntrypointAction(cmd.Action, "snapshot"):
		return nil, ErrNotImplemented
	case strings.HasPrefix(cmd.Action, "media."), strings.HasPrefix(cmd.Action, "webrtc."), strings.HasPrefix(cmd.Action, "system."):
		return nil, ErrNotImplemented
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

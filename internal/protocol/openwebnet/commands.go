package openwebnet

import (
	"fmt"
	"strings"
)

type CommandSet struct {
	Mute             string
	Unmute           string
	VoicemailEnable  string
	VoicemailDisable string
	StreamStop       string
}

func DefaultCommandSet() CommandSet {
	return CommandSet{
		Mute:             FrameMuteCommand,
		Unmute:           FrameUnmuteCommand,
		VoicemailEnable:  FrameVoicemailEnable,
		VoicemailDisable: FrameVoicemailDisable,
		StreamStop:       FrameStop,
	}
}

func ValidateCommand(frame string) error {
	f := strings.TrimSpace(frame)
	if f == "" {
		return fmt.Errorf("empty frame")
	}
	if !strings.HasPrefix(f, "*") || !strings.HasSuffix(f, "##") {
		return fmt.Errorf("invalid frame format")
	}
	return nil
}

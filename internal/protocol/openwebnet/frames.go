package openwebnetproto

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	FrameACK                 = "*#*1##"
	FrameStop                = "*7*0*##"
	FrameStreamProbe         = "*7*73#0#0*##"
	FrameAudioStatusCmd      = "*#8**33##"
	FrameAudioMuteCmd        = "*#8**#33*0##"
	FrameAudioUnmuteCmd      = "*#8**#33*1##"
	FrameAudioMuted          = "*#8**33*0##"
	FrameAudioUnmuted        = "*#8**33*1##"
	FrameVoicemailStatusCmd  = "*#8**40##"
	FrameVoicemailEnableCmd  = "*8*91##"
	FrameVoicemailDisableCmd = "*8*92##"
)

var voicemailStatusFrameRegexp = regexp.MustCompile(`^\*#8\*\*40\*([01])\*([01])(?:\*.*)?##$`)

func BuildUnlockOpen(devAddr string) string {
	return fmt.Sprintf("*8*19*%s##", strings.TrimSpace(devAddr))
}

func BuildUnlockClose(devAddr string) string {
	return fmt.Sprintf("*8*20*%s##", strings.TrimSpace(devAddr))
}

func BuildStreamStartVideo(port int) string {
	return fmt.Sprintf("*7*300#127#0#0#1#%d#0*##", port)
}

func BuildStreamStartAudio(port int) string {
	return fmt.Sprintf("*7*300#127#0#0#1#%d#2*##", port)
}

func IsACK(frame string) bool {
	return strings.TrimSpace(frame) == FrameACK
}

func IsRingStart(frame string) bool {
	return strings.HasPrefix(strings.TrimSpace(frame), "*8*1#1#4#")
}

func IsViewRequest(frame string) bool {
	return strings.HasPrefix(strings.TrimSpace(frame), "*8*1#5#4#")
}

func IsFloorRingStart(frame string) bool {
	return strings.HasPrefix(strings.TrimSpace(frame), "*7*59#")
}

func IsUnlockOpen(frame string) bool {
	return strings.HasPrefix(strings.TrimSpace(frame), "*8*19*")
}

func IsUnlockClose(frame string) bool {
	return strings.HasPrefix(strings.TrimSpace(frame), "*8*20*")
}

func IsStreamStartVideo(frame string) bool {
	f := strings.TrimSpace(frame)
	return strings.HasPrefix(f, "*7*300#") && strings.Contains(f, "#5007#")
}

func IsStreamStartAudio(frame string) bool {
	f := strings.TrimSpace(frame)
	return strings.HasPrefix(f, "*7*300#") && strings.Contains(f, "#5000#2*##")
}

func IsStreamStop(frame string) bool {
	return strings.TrimSpace(frame) == FrameStop
}

func IsStreamProbe(frame string) bool {
	return strings.TrimSpace(frame) == FrameStreamProbe
}

func ParseVoicemailStatus(frame string) (enabled bool, welcomeMessageEnabled bool, ok bool) {
	matches := voicemailStatusFrameRegexp.FindStringSubmatch(strings.TrimSpace(frame))
	if len(matches) < 3 {
		return false, false, false
	}
	enabled = matches[1] == "1"
	welcomeMessageEnabled = matches[2] == "1"
	return enabled, welcomeMessageEnabled, true
}

func ExtractAddress(frame string) string {
	if IsViewRequest(frame) {
		if addr := extractViewRequestAddress(frame); addr != "" {
			return addr
		}
	}

	trimmed := strings.TrimSpace(frame)
	trimmed = strings.TrimPrefix(trimmed, "*")
	trimmed = strings.TrimSuffix(trimmed, "##")
	parts := strings.Split(trimmed, "*")
	if len(parts) < 3 {
		return ""
	}
	return strings.TrimSpace(parts[2])
}

func extractViewRequestAddress(frame string) string {
	trimmed := strings.TrimSpace(frame)
	trimmed = strings.TrimPrefix(trimmed, "*")
	trimmed = strings.TrimSuffix(trimmed, "##")
	parts := strings.Split(trimmed, "*")
	if len(parts) < 2 {
		return ""
	}
	segment := strings.TrimSpace(parts[1])
	if segment == "" {
		return ""
	}
	idx := strings.LastIndex(segment, "#")
	if idx < 0 || idx+1 >= len(segment) {
		return ""
	}
	return strings.TrimSpace(segment[idx+1:])
}

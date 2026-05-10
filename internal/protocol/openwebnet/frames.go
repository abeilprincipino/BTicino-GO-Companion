package openwebnetproto

import (
	"fmt"
	"strings"
)

const (
	FrameACK          = "*#*1##"
	FrameStop         = "*7*0*##"
	FrameStreamProbe  = "*7*73#0#0*##"
	FrameAudioMuted   = "*#8**33*0##"
	FrameAudioUnmuted = "*#8**33*1##"
)

func BuildUnlockOpen(devAddr string) string {
	return fmt.Sprintf("*8*19*%s##", strings.TrimSpace(devAddr))
}

func BuildUnlockClose(devAddr string) string {
	return fmt.Sprintf("*8*20*%s##", strings.TrimSpace(devAddr))
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

func ExtractAddress(frame string) string {
	trimmed := strings.TrimSpace(frame)
	trimmed = strings.TrimPrefix(trimmed, "*")
	trimmed = strings.TrimSuffix(trimmed, "##")
	parts := strings.Split(trimmed, "*")
	if len(parts) < 3 {
		return ""
	}
	return strings.TrimSpace(parts[2])
}

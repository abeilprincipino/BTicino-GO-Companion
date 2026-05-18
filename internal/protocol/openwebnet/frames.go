package openwebnetproto

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	FrameACK                 = "*#*1##"
	FrameNACK                = "*#*0##"
	FrameSessionStartCmd     = "*99*0##"
	FrameAuthRequired        = "*98*2##"
	FrameStop                = "*7*0*##"
	FrameFreeAVResources     = "*7*9**##"
	FrameStreamProbe         = "*7*73#0#0*##"
	FrameAudioStatusCmd      = "*#8**33##"
	FrameAudioMuteCmd        = "*#8**#33*0##"
	FrameAudioUnmuteCmd      = "*#8**#33*1##"
	FrameAudioMuted          = "*#8**33*0##"
	FrameAudioUnmuted        = "*#8**33*1##"
	FrameVoicemailStatusCmd  = "*#8**40##"
	FrameVoicemailEnableCmd  = "*8*91##"
	FrameVoicemailDisableCmd = "*8*92##"
	FrameDiagIPCmd           = "*#13**10##"
	FrameDiagNetmaskCmd      = "*#13**11##"
	FrameDiagMACCmd          = "*#13**12##"
	FrameDiagFirmwareCmd     = "*#13**16##"
	FrameDiagHardwareCmd     = "*#13**17##"
	FrameDiagKernelCmd       = "*#13**23##"
	FrameDiagDistributionCmd = "*#13**24##"
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

func BuildReceiveVideo(where string) string {
	return fmt.Sprintf("*7*0*%s##", strings.TrimSpace(where))
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
	channel, ok := parseStreamStartChannel(frame)
	return ok && (channel == "0" || channel == "1")
}

func IsStreamStartAudio(frame string) bool {
	channel, ok := parseStreamStartChannel(frame)
	return ok && channel == "2"
}

func IsStreamStop(frame string) bool {
	return strings.TrimSpace(frame) == FrameStop
}

func IsFreeAVResources(frame string) bool {
	return strings.TrimSpace(frame) == FrameFreeAVResources
}

func ParseReceiveVideoWhere(frame string) (string, bool) {
	f := strings.TrimSpace(frame)
	if f == FrameStop || !strings.HasPrefix(f, "*7*0*") || !strings.HasSuffix(f, "##") {
		return "", false
	}
	where := strings.TrimSuffix(strings.TrimPrefix(f, "*7*0*"), "##")
	where = strings.TrimSpace(where)
	if where == "" {
		return "", false
	}
	return where, true
}

func IsReceiveVideo(frame string) bool {
	_, ok := ParseReceiveVideoWhere(frame)
	return ok
}

func IsStreamProbe(frame string) bool {
	return strings.TrimSpace(frame) == FrameStreamProbe
}

func parseStreamStartChannel(frame string) (string, bool) {
	f := strings.TrimSpace(frame)
	if !strings.HasPrefix(f, "*7*300#") || !strings.HasSuffix(f, "*##") {
		return "", false
	}
	trimmed := strings.TrimPrefix(f, "*7*300#")
	trimmed = strings.TrimSuffix(trimmed, "*##")
	parts := strings.Split(trimmed, "#")
	if len(parts) < 2 {
		return "", false
	}
	channel := strings.TrimSpace(parts[len(parts)-1])
	if channel == "" {
		return "", false
	}
	return channel, true
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

func IsVoicemailStatus(frame string) bool {
	_, _, ok := ParseVoicemailStatus(frame)
	return ok
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

func ParseDiagnosticReply(frame string) (code string, values []string, ok bool) {
	trimmed := strings.TrimSpace(frame)
	if !strings.HasPrefix(trimmed, "*#13**") || !strings.HasSuffix(trimmed, "##") {
		return "", nil, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(trimmed, "*#13**"), "##")
	if body == "" {
		return "", nil, false
	}
	parts := strings.Split(body, "*")
	if len(parts) == 0 {
		return "", nil, false
	}
	code = strings.TrimSpace(parts[0])
	if code == "" {
		return "", nil, false
	}
	values = make([]string, 0, len(parts)-1)
	for _, part := range parts[1:] {
		values = append(values, strings.TrimSpace(part))
	}
	return code, values, true
}

func ParseDiagnosticIP(frame string) (string, bool) {
	return parseDiagnosticDotString(frame, "10")
}

func ParseDiagnosticNetmask(frame string) (string, bool) {
	return parseDiagnosticDotString(frame, "11")
}

func ParseDiagnosticFirmware(frame string) (string, bool) {
	return parseDiagnosticDotString(frame, "16")
}

func ParseDiagnosticHardware(frame string) (string, bool) {
	return parseDiagnosticDotString(frame, "17")
}

func ParseDiagnosticKernel(frame string) (string, bool) {
	return parseDiagnosticDotString(frame, "23")
}

func ParseDiagnosticDistribution(frame string) (string, bool) {
	return parseDiagnosticDotString(frame, "24")
}

func ParseDiagnosticMAC(frame string) (string, bool) {
	code, values, ok := ParseDiagnosticReply(frame)
	if !ok || code != "12" || len(values) != 6 {
		return "", false
	}
	var parts [6]string
	for i, raw := range values {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 || v > 255 {
			return "", false
		}
		parts[i] = fmt.Sprintf("%02x", v)
	}
	return strings.Join(parts[:], ":"), true
}

func parseDiagnosticDotString(frame string, expectedCode string) (string, bool) {
	code, values, ok := ParseDiagnosticReply(frame)
	if !ok || code != expectedCode || len(values) == 0 {
		return "", false
	}
	out := strings.Join(values, ".")
	out = strings.TrimSpace(strings.Trim(out, "."))
	if out == "" {
		return "", false
	}
	return out, true
}

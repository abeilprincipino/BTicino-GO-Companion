package openwebnet

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

var (
	voicemailStatusFrameRegexp = regexp.MustCompile(`^\*#8\*\*40\*([01])\*([01])(?:\*.*)?##$`)
	ringIdentityFrameRegexp    = regexp.MustCompile(`^\*8\*9#1#4\*([^#*]+)#2##$`)
)

func BuildUnlockOpen(devAddr string) string {
	return fmt.Sprintf("*8*19*%s##", strings.TrimSpace(devAddr))
}

func BuildUnlockClose(devAddr string) string {
	return fmt.Sprintf("*8*20*%s##", strings.TrimSpace(devAddr))
}

func BuildAVAddStreamVideo(ip string, port int, highRes bool) string {
	quality := "1"
	if highRes {
		quality = "0"
	}
	return fmt.Sprintf("*7*300#%s#%d#%s*##", encodeIPHashForm(ip), port, quality)
}

func BuildAVAddStreamAudio(ip string, port int) string {
	return fmt.Sprintf("*7*300#%s#%d#2*##", encodeIPHashForm(ip), port)
}

func BuildStreamStartVideo(port int) string {
	return BuildAVAddStreamVideo("127.0.0.1", port, true)
}

func BuildStreamStartAudio(port int) string {
	return BuildAVAddStreamAudio("127.0.0.1", port)
}

func IsRingStart(frame string) bool {
	return strings.HasPrefix(strings.TrimSpace(frame), "*8*1#1#4#")
}

func IsViewRequest(frame string) bool {
	return strings.HasPrefix(strings.TrimSpace(frame), "*8*1#5#4#")
}

func IsUnmappedRingFrame(frame string) bool {
	return strings.HasPrefix(strings.TrimSpace(frame), "*7*59#")
}

func ParseRingIdentityAddress(frame string) (string, bool) {
	matches := ringIdentityFrameRegexp.FindStringSubmatch(strings.TrimSpace(frame))
	if len(matches) < 2 {
		return "", false
	}
	address := strings.TrimSpace(matches[1])
	if address == "" {
		return "", false
	}
	return address, true
}

func IsRingIdentity(frame string) bool {
	_, ok := ParseRingIdentityAddress(frame)
	return ok
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
	trimmed := strings.TrimSpace(frame)
	if trimmed == FrameStop || !strings.HasPrefix(trimmed, "*7*0*") || !strings.HasSuffix(trimmed, "##") {
		return "", false
	}
	where := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "*7*0*"), "##"))
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

func ParseVoicemailStatus(frame string) (enabled bool, welcomeMessageEnabled bool, ok bool) {
	matches := voicemailStatusFrameRegexp.FindStringSubmatch(strings.TrimSpace(frame))
	if len(matches) < 3 {
		return false, false, false
	}
	return matches[1] == "1", matches[2] == "1", true
}

func IsVoicemailStatus(frame string) bool {
	_, _, ok := ParseVoicemailStatus(frame)
	return ok
}

func ExtractAddress(frame string) string {
	if address, ok := ParseRingIdentityAddress(frame); ok {
		return address
	}
	if IsViewRequest(frame) {
		if address := extractViewRequestAddress(frame); address != "" {
			return address
		}
	}

	trimmed := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(frame), "*"), "##")
	parts := strings.Split(trimmed, "*")
	if len(parts) < 3 {
		return ""
	}
	return strings.TrimSpace(parts[2])
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

func ParseDiagnosticIP(frame string) (string, bool) { return parseDiagnosticDotString(frame, "10") }

func ParseDiagnosticNetmask(frame string) (string, bool) {
	return parseDiagnosticDotString(frame, "11")
}

func ParseDiagnosticFirmware(frame string) (string, bool) {
	return parseDiagnosticDotString(frame, "16")
}

func ParseDiagnosticHardware(frame string) (string, bool) {
	return parseDiagnosticDotString(frame, "17")
}

func ParseDiagnosticKernel(frame string) (string, bool) { return parseDiagnosticDotString(frame, "23") }

func ParseDiagnosticDistribution(frame string) (string, bool) {
	return parseDiagnosticDotString(frame, "24")
}

func ParseDiagnosticMAC(frame string) (string, bool) {
	code, values, ok := ParseDiagnosticReply(frame)
	if !ok || code != "12" || len(values) != 6 {
		return "", false
	}

	var parts [6]string
	for index, raw := range values {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > 255 {
			return "", false
		}
		parts[index] = fmt.Sprintf("%02x", value)
	}
	return strings.Join(parts[:], ":"), true
}

func encodeIPHashForm(ip string) string {
	return strings.ReplaceAll(strings.TrimSpace(ip), ".", "#")
}

func parseStreamStartChannel(frame string) (string, bool) {
	trimmed := strings.TrimSpace(frame)
	if !strings.HasPrefix(trimmed, "*7*300#") || !strings.HasSuffix(trimmed, "*##") {
		return "", false
	}
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(trimmed, "*7*300#"), "*##"), "#")
	if len(parts) < 2 {
		return "", false
	}
	channel := strings.TrimSpace(parts[len(parts)-1])
	return channel, channel != ""
}

func extractViewRequestAddress(frame string) string {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(frame), "*"), "##")
	parts := strings.Split(trimmed, "*")
	if len(parts) < 2 {
		return ""
	}
	segment := strings.TrimSpace(parts[1])
	index := strings.LastIndex(segment, "#")
	if index < 0 || index+1 >= len(segment) {
		return ""
	}
	return strings.TrimSpace(segment[index+1:])
}

func parseDiagnosticDotString(frame string, expectedCode string) (string, bool) {
	code, values, ok := ParseDiagnosticReply(frame)
	if !ok || code != expectedCode || len(values) == 0 {
		return "", false
	}
	value := strings.TrimSpace(strings.Trim(strings.Join(values, "."), "."))
	return value, value != ""
}

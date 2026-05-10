package sipprotocol

import (
	"fmt"
	"strings"
)

type SDPConfig struct {
	Host           string
	AudioPort      int
	VideoPort      int
	IncludeDevAddr bool
	DevAddr        string
}

func BuildOffer(cfg SDPConfig) string {
	host := normalizeHost(cfg.Host)
	audioPort := normalizeAudioPort(cfg.AudioPort)
	videoPort := normalizeVideoPort(cfg.VideoPort)

	audioLines := []string{
		fmt.Sprintf("m=audio %d RTP/SAVP 110", audioPort),
		"a=rtpmap:110 speex/8000",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:dummykey",
	}
	if cfg.IncludeDevAddr {
		devaddr := strings.TrimSpace(cfg.DevAddr)
		if devaddr != "" {
			audioLines = append([]string{"a=DEVADDR:" + devaddr}, audioLines...)
		}
	}

	lines := []string{
		"v=0",
		"o=companion 3748 462 IN IP4 " + host,
		"s=Companion",
		"c=IN IP4 " + host,
		"t=0 0",
	}
	lines = append(lines, audioLines...)
	lines = append(lines,
		fmt.Sprintf("m=video %d RTP/SAVP 96", videoPort),
		"a=rtpmap:96 H264/90000",
		"a=fmtp:96 profile-level-id=42801F",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:dummykey",
		"a=recvonly",
	)
	return strings.Join(lines, "\r\n") + "\r\n"
}

func BuildAnswer(cfg SDPConfig) string {
	host := normalizeHost(cfg.Host)
	audioPort := normalizeAudioPort(cfg.AudioPort)
	videoPort := normalizeVideoPort(cfg.VideoPort)
	lines := []string{
		"v=0",
		"o=companion 3747 461 IN IP4 " + host,
		"s=Companion",
		"c=IN IP4 " + host,
		"t=0 0",
		fmt.Sprintf("m=audio %d RTP/SAVP 110", audioPort),
		"a=rtpmap:110 speex/8000",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:dummykey",
		fmt.Sprintf("m=video %d RTP/SAVP 96", videoPort),
		"a=rtpmap:96 H264/90000",
		"a=fmtp:96 profile-level-id=42801F",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:dummykey",
		"a=recvonly",
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

func normalizeHost(host string) string {
	h := strings.TrimSpace(host)
	if h == "" || h == "0.0.0.0" {
		return "127.0.0.1"
	}
	return h
}

func normalizeAudioPort(port int) int {
	if port <= 0 {
		return 5000
	}
	return port
}

func normalizeVideoPort(port int) int {
	if port <= 0 {
		return 5007
	}
	return port
}

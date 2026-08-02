package signaling

import (
	"fmt"
	"strings"
)

const (
	// SIP requires media sections, but the intercom sends the usable clear RTP
	// through separate OpenWebNet AV requests, to whichever loopback ports the
	// receivers bound — see media.AVPorts. These two numbers carry no traffic.
	AudioSDPPort = 65000
	VideoSDPPort = 65002
)

func BuildOffer(host, devAddr string) string {
	lines := sessionLines(host, "3748", "462")

	devAddr = strings.TrimSpace(devAddr)
	if devAddr != "" {
		lines = append(lines, "a=DEVADDR:"+devAddr)
	}

	lines = append(lines, mediaLines()...)

	return strings.Join(lines, "\r\n") + "\r\n"
}

func BuildAnswer(host, devAddr string) string {
	lines := sessionLines(host, "3747", "461")

	devAddr = strings.TrimSpace(devAddr)
	if devAddr != "" {
		lines = append(lines, "a=DEVADDR:"+devAddr)
	}

	lines = append(lines, mediaLines()...)

	return strings.Join(lines, "\r\n") + "\r\n"
}

func sessionLines(host, sessionID, sessionVersion string) []string {
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}

	return []string{
		"v=0",
		fmt.Sprintf("o=companion %s %s IN IP4 %s", sessionID, sessionVersion, host),
		"s=Companion",
		"c=IN IP4 " + host,
		"t=0 0",
	}
}

func mediaLines() []string {
	return []string{
		fmt.Sprintf("m=audio %d RTP/SAVP 110", AudioSDPPort),
		"a=rtpmap:110 speex/8000",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:dummykey",
		fmt.Sprintf("m=video %d RTP/SAVP 96", VideoSDPPort),
		"a=rtpmap:96 H264/90000",
		"a=fmtp:96 profile-level-id=42801F",
		"a=crypto:1 AES_CM_128_HMAC_SHA1_80 inline:dummykey",
		"a=recvonly",
	}
}

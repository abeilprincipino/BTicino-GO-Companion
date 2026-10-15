package signaling

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	// SIP requires media sections, but the intercom sends the usable clear RTP
	// through separate OpenWebNet AV requests, to whichever loopback ports the
	// receivers bound — see media.AVPorts. Nothing is received on these two.
	//
	// AudioSDPPort does carry outbound traffic: it is the source port of the
	// media keepalive (keepalive.go), which must come from the port we
	// advertised for the far end to accept it as our stream.
	AudioSDPPort = 65000
	VideoSDPPort = 65002
)

var errNoRemoteAudioTarget = errors.New("sip: remote sdp has no usable audio target")

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

// parseRemoteAudioTarget returns where the far end receives audio: the port of
// its first audio media line and the connection address that applies to it,
// the media-level c= line when it has one, the session-level one otherwise.
func parseRemoteAudioTarget(sdp string) (*net.UDPAddr, error) {
	var (
		sessionHost string
		mediaHost   string
		port        int
		inAudio     bool
		found       bool
	)

	for _, raw := range strings.Split(sdp, "\n") {
		line := strings.TrimSpace(raw)

		switch {
		case strings.HasPrefix(line, "m="):
			if found {
				// Only the first audio section counts; the rest of the SDP is
				// another media's business.
				inAudio = false

				continue
			}

			fields := strings.Fields(line[2:])
			inAudio = len(fields) >= 2 && fields[0] == "audio"

			if inAudio {
				p, err := strconv.Atoi(fields[1])
				if err != nil || p <= 0 || p > 65535 {
					inAudio = false

					continue
				}

				port = p
				found = true
			}
		case strings.HasPrefix(line, "c="):
			host := parseConnectionHost(line)

			switch {
			case inAudio:
				mediaHost = host
			case !found:
				sessionHost = host
			}
		}
	}

	host := mediaHost
	if host == "" {
		host = sessionHost
	}

	if !found || host == "" {
		return nil, errNoRemoteAudioTarget
	}

	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return nil, errNoRemoteAudioTarget
	}

	return &net.UDPAddr{IP: ip, Port: port}, nil
}

// parseConnectionHost returns the address of a "c=IN IP4 <addr>[/ttl]" line,
// or "" when the line has none.
func parseConnectionHost(line string) string {
	if !strings.HasPrefix(line, "c=") {
		return ""
	}

	fields := strings.Fields(line[2:])
	if len(fields) < 3 {
		return ""
	}

	host, _, _ := strings.Cut(fields[2], "/")

	return strings.TrimSpace(host)
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

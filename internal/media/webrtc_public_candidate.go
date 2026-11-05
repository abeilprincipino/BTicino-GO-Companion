package media

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const (
	iceCandidatePrefix = "candidate:"
	// maxICEFoundationLength is the foundation limit of the ICE grammar (RFC 8839).
	maxICEFoundationLength = 32
	publicMuxFoundationTag = "p"
)

// Field positions of an ICE candidate attribute: foundation, component,
// transport, priority, address, port, "typ", type, then name-value pairs.
const (
	candidateFoundationField = 0
	candidateTransportField  = 2
	candidateAddressField    = 4
	candidatePortField       = 5
	candidateTypKeywordField = 6
	candidateTypeField       = 7
	candidateMinFields       = 8
)

// publicMuxCandidate derives the candidate that announces the ICE mux port on
// the public address of a server-reflexive candidate. A home router that
// forwards UDP 8555 to the intercom then lets a peer off the LAN reach the mux,
// where pion answers its checks as a peer-reflexive pair. It returns false for
// any other candidate, including ones it cannot parse.
func publicMuxCandidate(candidate string) (string, bool) {
	body, hasPrefix := strings.CutPrefix(strings.TrimSpace(candidate), iceCandidatePrefix)
	fields := strings.Fields(body)

	if !isPublicMuxSource(fields) {
		return "", false
	}

	muxPort := strconv.Itoa(webrtcICEPort)
	derived := slices.Clone(fields)
	derived[candidateFoundationField] = publicMuxFoundation(fields[candidateFoundationField])
	derived[candidatePortField] = muxPort

	for i := candidateMinFields; i+1 < len(derived); i += 2 {
		if derived[i] == "rport" {
			derived[i+1] = muxPort
		}
	}

	result := strings.Join(derived, " ")

	if hasPrefix {
		return iceCandidatePrefix + result, true
	}

	return result, true
}

// isPublicMuxSource reports whether fields are a UDP IPv4 server-reflexive
// candidate on a port other than the mux one.
func isPublicMuxSource(fields []string) bool {
	if len(fields) < candidateMinFields ||
		!strings.EqualFold(fields[candidateTransportField], "udp") ||
		fields[candidateTypKeywordField] != "typ" ||
		fields[candidateTypeField] != "srflx" {
		return false
	}

	address, err := netip.ParseAddr(fields[candidateAddressField])
	if err != nil || !address.Is4() {
		return false
	}

	port, err := strconv.ParseUint(fields[candidatePortField], 10, 16)

	return err == nil && port != webrtcICEPort
}

// publicMuxFoundation gives the derived candidate its own foundation, so the
// peer does not pair it as a copy of the random-port candidate.
func publicMuxFoundation(foundation string) string {
	if len(foundation) < maxICEFoundationLength {
		return foundation + publicMuxFoundationTag
	}

	return foundation[:maxICEFoundationLength-1] + publicMuxFoundationTag
}

// publicMuxAnnouncer derives the mux candidate once per public address. The
// zero value is ready to use.
type publicMuxAnnouncer struct {
	mu        sync.Mutex
	announced map[string]struct{}
}

func (a *publicMuxAnnouncer) derive(candidate string) (string, bool) {
	derived, ok := publicMuxCandidate(candidate)
	if !ok {
		return "", false
	}

	body, _ := strings.CutPrefix(derived, iceCandidatePrefix)
	address := strings.Fields(body)[candidateAddressField]

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, done := a.announced[address]; done {
		return "", false
	}

	if a.announced == nil {
		a.announced = make(map[string]struct{})
	}

	a.announced[address] = struct{}{}

	return derived, true
}

// withPublicMuxCandidates adds, after each server-reflexive a=candidate line,
// the candidate derived by publicMuxCandidate, once per public address and
// media section. It serves the answers of clients that do not trickle.
func withPublicMuxCandidates(sdp string) string {
	eol := "\n"

	if strings.Contains(sdp, "\r\n") {
		eol = "\r\n"
	}

	lines := strings.Split(sdp, eol)
	out := make([]string, 0, len(lines))
	section := &publicMuxAnnouncer{}

	for _, line := range lines {
		if strings.HasPrefix(line, "m=") {
			section = &publicMuxAnnouncer{}
		}

		out = append(out, line)

		attribute, isAttribute := strings.CutPrefix(line, "a=")
		if !isAttribute {
			continue
		}

		if derived, ok := section.derive(attribute); ok {
			out = append(out, "a="+derived)
		}
	}

	return strings.Join(out, eol)
}

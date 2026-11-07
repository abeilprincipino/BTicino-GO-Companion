package media

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/pion/webrtc/v4"
)

// maxLoggedCandidatePairs bounds the pair lines of one session; a browser with
// several interfaces and a TURN server yields a few dozen pairs at most.
const maxLoggedCandidatePairs = 48

// sharedAddressSpace is the carrier-grade NAT range (RFC 6598).
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// candidateSummary describes a candidate without its address, so a log that
// users share does not carry their IPs: the family and the scope are enough to
// tell an IPv6-only phone, a carrier NAT or a LAN path apart.
type candidateSummary struct {
	typ      string
	protocol string
	family   string
	scope    string
	port     string
}

func (c candidateSummary) mdns() bool {
	return c.family == "mdns"
}

// describeAddress gives the family (ipv4, ipv6, mdns) and the scope (public,
// private, cgnat, link-local, loopback...) of a candidate address.
func describeAddress(address string) (family, scope string) {
	if strings.HasSuffix(address, ".local") {
		return "mdns", "mdns"
	}

	addr, err := netip.ParseAddr(address)
	if err != nil {
		return "unknown", "unknown"
	}

	addr = addr.Unmap()
	family = "ipv6"

	if addr.Is4() {
		family = "ipv4"
	}

	switch {
	case addr.IsUnspecified():
		scope = "unspecified"
	case addr.IsLoopback():
		scope = "loopback"
	case addr.IsLinkLocalUnicast():
		scope = "link-local"
	case addr.IsPrivate():
		scope = "private"
	case sharedAddressSpace.Contains(addr):
		scope = "cgnat"
	case addr.IsGlobalUnicast():
		scope = "public"
	default:
		scope = "other"
	}

	return family, scope
}

// describeCandidate reads a candidate line, "candidate:<foundation> <component>
// <protocol> <priority> <address> <port> typ <type> ...".
func describeCandidate(candidate string) candidateSummary {
	body, _ := strings.CutPrefix(strings.TrimSpace(candidate), iceCandidatePrefix)

	fields := strings.Fields(body)
	if len(fields) < candidateMinFields {
		return candidateSummary{}
	}

	family, scope := describeAddress(fields[candidateAddressField])

	return candidateSummary{
		typ:      fields[candidateTypeField],
		protocol: strings.ToLower(fields[candidateTransportField]),
		family:   family,
		scope:    scope,
		port:     fields[candidatePortField],
	}
}

func describeCandidateStats(stats webrtc.ICECandidateStats) candidateSummary {
	family, scope := describeAddress(stats.IP)

	return candidateSummary{
		typ:      stats.CandidateType.String(),
		protocol: strings.ToLower(stats.Protocol),
		family:   family,
		scope:    scope,
		port:     strconv.Itoa(int(stats.Port)),
	}
}

// candidatePairSummary is one ICE candidate pair at the end of a session. Its
// counters tell a check that never reached the intercom (requests_received=0)
// from one that arrived but got no answer through.
type candidatePairSummary struct {
	id                string
	state             string
	nominated         bool
	local             candidateSummary
	remote            candidateSummary
	requestsSent      uint64
	requestsReceived  uint64
	responsesSent     uint64
	responsesReceived uint64
}

func candidatePairSummaries(report webrtc.StatsReport) []candidatePairSummary {
	candidates := make(map[string]webrtc.ICECandidateStats)
	pairs := make([]webrtc.ICECandidatePairStats, 0)

	for _, stats := range report {
		switch typed := stats.(type) {
		case webrtc.ICECandidateStats:
			candidates[typed.ID] = typed
		case webrtc.ICECandidatePairStats:
			pairs = append(pairs, typed)
		}
	}

	slices.SortFunc(pairs, func(a, b webrtc.ICECandidatePairStats) int {
		return strings.Compare(a.ID, b.ID)
	})

	summaries := make([]candidatePairSummary, 0, len(pairs))

	for _, pair := range pairs {
		summaries = append(summaries, candidatePairSummary{
			id:                pair.ID,
			state:             string(pair.State),
			nominated:         pair.Nominated,
			local:             describeCandidateStats(candidates[pair.LocalCandidateID]),
			remote:            describeCandidateStats(candidates[pair.RemoteCandidateID]),
			requestsSent:      pair.RequestsSent,
			requestsReceived:  pair.RequestsReceived,
			responsesSent:     pair.ResponsesSent,
			responsesReceived: pair.ResponsesReceived,
		})
	}

	return summaries
}

// logCandidatePairs records every ICE pair of a closing session, so a session
// that never connects shows which paths were tried and how far each one got.
func (s *WebRTCService) logCandidatePairs(session *webRTCSession) {
	pairs := candidatePairSummaries(session.pc.GetStats())

	s.logger.Debug("webrtc ICE candidate pairs", "session_id", session.id, "entrypoint_id", session.entrypointID, "count", len(pairs))

	for i, pair := range pairs {
		if i == maxLoggedCandidatePairs {
			s.logger.Debug("webrtc ICE candidate pairs truncated", "session_id", session.id, "entrypoint_id", session.entrypointID, "omitted", len(pairs)-i)

			return
		}

		s.logger.Debug("webrtc ICE candidate pair",
			"session_id", session.id,
			"entrypoint_id", session.entrypointID,
			"state", pair.state,
			"nominated", pair.nominated,
			"local_type", pair.local.typ,
			"local_family", pair.local.family,
			"local_scope", pair.local.scope,
			"local_port", pair.local.port,
			"remote_type", pair.remote.typ,
			"remote_family", pair.remote.family,
			"remote_scope", pair.remote.scope,
			"remote_port", pair.remote.port,
			"requests_sent", pair.requestsSent,
			"requests_received", pair.requestsReceived,
			"responses_sent", pair.responsesSent,
			"responses_received", pair.responsesReceived,
		)
	}
}

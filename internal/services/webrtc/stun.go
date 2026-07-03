package webrtc

import (
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/stun/v3"
)

const (
	// publicCandidateTTL bounds how long a STUN-discovered public mapping is
	// reused. Kept short because carrier-grade NAT mappings for the media
	// socket can change; a burst of offers still hits STUN at most once per TTL.
	publicCandidateTTL = 30 * time.Second
	// stunDiscoveryDeadline caps a single GetXORMappedAddr binding exchange.
	stunDiscoveryDeadline = 2 * time.Second
	defaultSTUNPort       = "3478"

	// RFC 8445 candidate priority components. The srflx type preference is
	// deliberately below the host type preference pion uses (126) so LAN
	// clients keep choosing the direct host path over the injected reflexive
	// candidate.
	srflxTypePreference      uint32 = 100
	candidateLocalPreference uint32 = 65535
	candidateComponentRTP    uint32 = 1
)

// xorMappedAddrProvider is satisfied by *ice.UniversalUDPMuxDefault. It is
// factored out so the discovery/caching logic can be unit-tested with a fake
// (no real STUN server / network).
type xorMappedAddrProvider interface {
	GetXORMappedAddr(serverAddr net.Addr, deadline time.Duration) (*stun.XORMappedAddress, error)
}

// publicMapping is the public address the media socket maps to, as seen by a
// STUN server.
type publicMapping struct {
	ip     string
	port   int
	server string
}

// stunDiscoverer discovers (and briefly caches) the public mapping of the muxed
// 8555 UDP socket by performing a STUN binding exchange THROUGH that same socket
// via the universal UDP mux. This mirrors go2rtc's "candidates: stun:8555"
// trick: the public address is learned out-of-band and injected as a candidate,
// rather than relying on pion's (unwired) srflx-over-mux gathering.
type stunDiscoverer struct {
	provider xorMappedAddrProvider
	servers  []string // resolved-form "host:port" dial targets
	ttl      time.Duration
	deadline time.Duration
	now      func() time.Time

	mu       sync.Mutex
	cached   *publicMapping
	cachedAt time.Time
}

// newStunDiscoverer builds a discoverer from the configured ICE server URLs,
// keeping only valid stun: entries. Returns nil when none parse (e.g. only
// turn: URLs), which disables injection cleanly.
func newStunDiscoverer(provider xorMappedAddrProvider, iceServers []string) *stunDiscoverer {
	servers := make([]string, 0, len(iceServers))
	for _, u := range iceServers {
		s, err := parseSTUNServer(u)
		if err != nil {
			continue
		}
		servers = append(servers, s)
	}
	if len(servers) == 0 {
		return nil
	}
	return &stunDiscoverer{
		provider: provider,
		servers:  servers,
		ttl:      publicCandidateTTL,
		deadline: stunDiscoveryDeadline,
		now:      time.Now,
	}
}

// parseSTUNServer extracts a "host:port" dial target from a "stun:host[:port]"
// URL, defaulting the port to 3478 when absent. It does not resolve DNS.
func parseSTUNServer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	rest, ok := strings.CutPrefix(raw, "stun:")
	if !ok {
		return "", fmt.Errorf("not a stun url: %q", raw)
	}
	rest = strings.TrimSpace(rest)
	if i := strings.IndexByte(rest, '?'); i >= 0 { // drop ?transport=... etc.
		rest = strings.TrimSpace(rest[:i])
	}
	if rest == "" {
		return "", fmt.Errorf("stun url missing host: %q", raw)
	}
	host, port, err := net.SplitHostPort(rest)
	if err != nil {
		// No port present: use the whole remainder as host with the default
		// port. Reject ambiguous unbracketed multi-colon forms (bare IPv6).
		if strings.Count(rest, ":") > 0 {
			return "", fmt.Errorf("invalid stun url %q: %w", raw, err)
		}
		host = rest
		port = defaultSTUNPort
	}
	if host == "" {
		return "", fmt.Errorf("stun url missing host: %q", raw)
	}
	return net.JoinHostPort(host, port), nil
}

// discover returns the cached public mapping when still fresh, otherwise
// performs a STUN binding exchange through the muxed socket against the first
// resolvable server. The returned duration is the remaining cache TTL.
func (d *stunDiscoverer) discover() (publicMapping, time.Duration, error) {
	now := time.Now
	if d.now != nil {
		now = d.now
	}

	d.mu.Lock()
	if d.cached != nil {
		if age := now().Sub(d.cachedAt); age < d.ttl {
			m := *d.cached
			remaining := d.ttl - age
			d.mu.Unlock()
			return m, remaining, nil
		}
	}
	d.mu.Unlock()

	var lastErr error
	for _, s := range d.servers {
		addr, err := net.ResolveUDPAddr("udp4", s)
		if err != nil {
			lastErr = err
			continue
		}
		xor, err := d.provider.GetXORMappedAddr(addr, d.deadline)
		if err != nil {
			lastErr = err
			continue
		}
		if xor == nil || xor.IP == nil {
			lastErr = errors.New("stun returned no mapped address")
			continue
		}
		m := publicMapping{ip: xor.IP.String(), port: xor.Port, server: s}
		d.mu.Lock()
		d.cached = &m
		d.cachedAt = now()
		d.mu.Unlock()
		return m, d.ttl, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no stun servers configured")
	}
	return publicMapping{}, 0, lastErr
}

// icePriority computes an ICE candidate priority per RFC 8445 §5.1.2.1.
func icePriority(typePref, localPref, component uint32) uint32 {
	return (typePref << 24) + (localPref << 8) + (256 - component)
}

// srflxFoundation returns a deterministic ICE foundation for the injected
// reflexive candidate, distinct from host-candidate foundations.
func srflxFoundation(ip string, port int) string {
	return fmt.Sprintf("%d", crc32.ChecksumIEEE([]byte(fmt.Sprintf("srflx-%s-%d", ip, port))))
}

// srflxCandidateLine builds an a=candidate line for a server-reflexive
// candidate. We encode it as typ srflx with raddr 0.0.0.0 rport 0 (the honest
// reflexive encoding; browsers accept it and treat it as a reflexive path with
// unknown base). The priority is below the host candidates', so LAN clients
// still prefer the direct route.
func srflxCandidateLine(ip string, port int, priority uint32) string {
	return fmt.Sprintf(
		"a=candidate:%s 1 udp %d %s %d typ srflx raddr 0.0.0.0 rport 0",
		srflxFoundation(ip, port), priority, ip, port,
	)
}

// injectCandidateIntoAnswer appends candidateLine into the first media section
// of a (CRLF) SDP answer. With a=group:BUNDLE the candidate applies
// session-wide (m-line index 0), which is why appending to the first section is
// sufficient. Returns the SDP unchanged when no m= section is present.
func injectCandidateIntoAnswer(sdp, candidateLine string) string {
	canon := canonicalizeSDP(sdp)
	if canon == "" {
		return sdp
	}
	lines := strings.Split(strings.TrimRight(canon, "\r\n"), "\r\n")

	firstM := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "m=") {
			firstM = i
			break
		}
	}
	if firstM < 0 {
		return sdp
	}

	// Insert immediately before a=end-of-candidates when present in this
	// section (RFC 8445 requires it be the last ICE-related line); otherwise
	// fall back to the section end, before the next m= line or at EOF.
	insertAt := len(lines)
	for i := firstM + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "m=") {
			insertAt = i
			break
		}
		if lines[i] == "a=end-of-candidates" {
			insertAt = i
			break
		}
	}

	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAt]...)
	out = append(out, candidateLine)
	out = append(out, lines[insertAt:]...)
	return strings.Join(out, "\r\n") + "\r\n"
}

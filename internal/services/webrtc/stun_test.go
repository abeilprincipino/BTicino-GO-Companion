package webrtc

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v3"
)

type fakeXORProvider struct {
	calls int
	ip    net.IP
	port  int
	err   error
}

func (f *fakeXORProvider) GetXORMappedAddr(net.Addr, time.Duration) (*stun.XORMappedAddress, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &stun.XORMappedAddress{IP: f.ip, Port: f.port}, nil
}

func TestParseSTUNServer(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "with port", in: "stun:stun.l.google.com:19302", want: "stun.l.google.com:19302"},
		{name: "without port defaults 3478", in: "stun:stun.example.org", want: "stun.example.org:3478"},
		{name: "surrounding spaces", in: "  stun:1.2.3.4:3478  ", want: "1.2.3.4:3478"},
		{name: "ipv6 bracketed", in: "stun:[2001:db8::1]:3478", want: "[2001:db8::1]:3478"},
		{name: "not a stun url", in: "turn:foo:3478", wantErr: true},
		{name: "no scheme", in: "stun.example.org:3478", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "missing host", in: "stun:", wantErr: true},
		{name: "too many colons", in: "stun:host:1:2", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSTUNServer(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("parseSTUNServer(%q)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewStunDiscovererFiltersInvalid(t *testing.T) {
	if d := newStunDiscoverer(&fakeXORProvider{}, []string{"turn:x:1", "not-a-url"}); d != nil {
		t.Fatalf("expected nil discoverer when no valid stun urls, got %+v", d)
	}
	d := newStunDiscoverer(&fakeXORProvider{}, []string{"turn:x:1", "stun:1.2.3.4:3478"})
	if d == nil || len(d.servers) != 1 || d.servers[0] != "1.2.3.4:3478" {
		t.Fatalf("expected one parsed server, got %+v", d)
	}
}

func TestStunDiscovererCachesWithinTTL(t *testing.T) {
	fake := &fakeXORProvider{ip: net.ParseIP("203.0.113.7"), port: 40000}
	current := time.Unix(1000, 0)
	d := &stunDiscoverer{
		provider: fake,
		servers:  []string{"127.0.0.1:3478"},
		ttl:      publicCandidateTTL,
		deadline: stunDiscoveryDeadline,
		now:      func() time.Time { return current },
	}

	m, cachedFor, err := d.discover()
	if err != nil {
		t.Fatalf("first discover: %v", err)
	}
	if m.ip != "203.0.113.7" || m.port != 40000 {
		t.Fatalf("unexpected mapping: %+v", m)
	}
	if cachedFor != publicCandidateTTL {
		t.Fatalf("fresh discovery cachedFor=%s want %s", cachedFor, publicCandidateTTL)
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 STUN call, got %d", fake.calls)
	}

	// Within TTL: served from cache, no new STUN call.
	current = current.Add(publicCandidateTTL / 2)
	if _, remaining, err := d.discover(); err != nil {
		t.Fatalf("cached discover: %v", err)
	} else if remaining != publicCandidateTTL-publicCandidateTTL/2 {
		t.Fatalf("cache-hit remaining=%s unexpected", remaining)
	}
	if fake.calls != 1 {
		t.Fatalf("cache hit must not call STUN, calls=%d", fake.calls)
	}

	// After TTL expiry: rediscover.
	current = current.Add(publicCandidateTTL)
	if _, _, err := d.discover(); err != nil {
		t.Fatalf("post-expiry discover: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("expected rediscovery after TTL, calls=%d", fake.calls)
	}
}

func TestStunDiscovererDiscoveryFailure(t *testing.T) {
	fake := &fakeXORProvider{err: errors.New("stun unreachable")}
	current := time.Unix(1000, 0)
	d := &stunDiscoverer{
		provider: fake,
		servers:  []string{"127.0.0.1:3478"},
		ttl:      publicCandidateTTL,
		deadline: stunDiscoveryDeadline,
		now:      func() time.Time { return current },
	}
	if _, _, err := d.discover(); err == nil {
		t.Fatalf("expected discovery error")
	}
}

func TestSrflxCandidatePriorityBelowHost(t *testing.T) {
	// pion host candidate priority (type pref 126, local pref 65535, component 1).
	const typicalHostPriority uint32 = (126 << 24) + (65535 << 8) + 255
	got := icePriority(srflxTypePreference, candidateLocalPreference, candidateComponentRTP)
	if got >= typicalHostPriority {
		t.Fatalf("srflx priority %d must be below host priority %d", got, typicalHostPriority)
	}
}

func TestInjectCandidateIntoAnswer(t *testing.T) {
	answer := "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"a=group:BUNDLE 0 1\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=mid:0\r\n" +
		"a=candidate:1 1 udp 2130706431 192.168.1.10 8555 typ host\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=mid:1\r\n"

	prio := icePriority(srflxTypePreference, candidateLocalPreference, candidateComponentRTP)
	line := srflxCandidateLine("203.0.113.7", 40000, prio)
	out := injectCandidateIntoAnswer(answer, line)

	lines := strings.Split(strings.TrimRight(out, "\r\n"), "\r\n")
	idxInjected, idxVideo, idxAudio := -1, -1, -1
	for i, l := range lines {
		switch {
		case l == line:
			idxInjected = i
		case strings.HasPrefix(l, "m=video"):
			idxVideo = i
		case strings.HasPrefix(l, "m=audio"):
			idxAudio = i
		}
	}
	if idxInjected < 0 {
		t.Fatalf("injected candidate line not found in output:\n%s", out)
	}
	if !(idxInjected > idxVideo && idxInjected < idxAudio) {
		t.Fatalf("injected candidate not in first media section (video=%d inj=%d audio=%d)", idxVideo, idxInjected, idxAudio)
	}
	// Exactly one injection.
	if strings.Count(out, line) != 1 {
		t.Fatalf("expected exactly one injected line, got %d", strings.Count(out, line))
	}
	// Format check.
	if !strings.HasPrefix(line, "a=candidate:") || !strings.Contains(line, " 1 udp ") ||
		!strings.Contains(line, " 203.0.113.7 40000 typ srflx raddr 0.0.0.0 rport 0") {
		t.Fatalf("unexpected candidate line format: %q", line)
	}
}

func TestInjectCandidateIntoAnswerBeforeEndOfCandidates(t *testing.T) {
	// Realistic pion answer: once ICE gathering completes, pion appends
	// a=end-of-candidates to each media section. The injected srflx candidate
	// must land BEFORE that marker (RFC 8445 requires end-of-candidates to be
	// the last ICE-related line in the section) and only in the first section.
	answer := "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n" +
		"a=group:BUNDLE 0 1\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=mid:0\r\n" +
		"a=candidate:1 1 udp 2130706431 192.168.1.10 8555 typ host\r\n" +
		"a=end-of-candidates\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=mid:1\r\n" +
		"a=candidate:1 1 udp 2130706431 192.168.1.10 8556 typ host\r\n" +
		"a=end-of-candidates\r\n"

	prio := icePriority(srflxTypePreference, candidateLocalPreference, candidateComponentRTP)
	line := srflxCandidateLine("203.0.113.7", 40000, prio)
	out := injectCandidateIntoAnswer(answer, line)

	lines := strings.Split(strings.TrimRight(out, "\r\n"), "\r\n")
	idxInjected, idxVideo, idxAudio := -1, -1, -1
	idxEndOfCandidates := make([]int, 0, 2)
	for i, l := range lines {
		switch {
		case l == line:
			idxInjected = i
		case strings.HasPrefix(l, "m=video"):
			idxVideo = i
		case strings.HasPrefix(l, "m=audio"):
			idxAudio = i
		case l == "a=end-of-candidates":
			idxEndOfCandidates = append(idxEndOfCandidates, i)
		}
	}
	if idxInjected < 0 {
		t.Fatalf("injected candidate line not found in output:\n%s", out)
	}
	if !(idxInjected > idxVideo && idxInjected < idxAudio) {
		t.Fatalf("injected candidate not in first media section (video=%d inj=%d audio=%d)", idxVideo, idxInjected, idxAudio)
	}
	if len(idxEndOfCandidates) != 2 {
		t.Fatalf("expected 2 end-of-candidates markers preserved, got %d", len(idxEndOfCandidates))
	}
	firstEndOfCandidates := idxEndOfCandidates[0]
	if !(idxInjected < firstEndOfCandidates) {
		t.Fatalf("injected candidate (line %d) must come before first section's end-of-candidates (line %d)", idxInjected, firstEndOfCandidates)
	}
	// Second section's end-of-candidates must be untouched (no injection there).
	secondEndOfCandidates := idxEndOfCandidates[1]
	if secondEndOfCandidates <= idxAudio {
		t.Fatalf("second end-of-candidates (line %d) should stay after m=audio (line %d)", secondEndOfCandidates, idxAudio)
	}
	// Exactly one injection overall.
	if strings.Count(out, line) != 1 {
		t.Fatalf("expected exactly one injected line, got %d", strings.Count(out, line))
	}
}

func TestInjectCandidateNoMediaSection(t *testing.T) {
	sdp := "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"
	out := injectCandidateIntoAnswer(sdp, "a=candidate:x")
	if strings.Contains(out, "a=candidate:x") {
		t.Fatalf("must not inject when no m= section present")
	}
}

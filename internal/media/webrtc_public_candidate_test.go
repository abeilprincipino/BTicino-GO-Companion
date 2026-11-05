package media

import (
	"strings"
	"testing"
)

func TestPublicMuxCandidate(t *testing.T) {
	longFoundation := strings.Repeat("a", 32)

	tests := []struct {
		name      string
		candidate string
		want      string
		wantOK    bool
	}{
		{
			name:      "srflx with prefix",
			candidate: "candidate:1918113218 1 udp 1694498815 203.0.113.7 51234 typ srflx raddr 192.168.101.2 rport 51234",
			want:      "candidate:1918113218p 1 udp 1694498815 203.0.113.7 8555 typ srflx raddr 192.168.101.2 rport 8555",
			wantOK:    true,
		},
		{
			name:      "srflx without prefix",
			candidate: "1918113218 1 udp 1694498815 203.0.113.7 51234 typ srflx raddr 192.168.101.2 rport 51234",
			want:      "1918113218p 1 udp 1694498815 203.0.113.7 8555 typ srflx raddr 192.168.101.2 rport 8555",
			wantOK:    true,
		},
		{
			name:      "srflx keeps extensions",
			candidate: "candidate:7 1 UDP 1694498815 203.0.113.7 51234 typ srflx raddr 0.0.0.0 rport 51234 generation 0 ufrag abcd",
			want:      "candidate:7p 1 UDP 1694498815 203.0.113.7 8555 typ srflx raddr 0.0.0.0 rport 8555 generation 0 ufrag abcd",
			wantOK:    true,
		},
		{
			name:      "srflx without raddr",
			candidate: "candidate:7 1 udp 1694498815 203.0.113.7 51234 typ srflx",
			want:      "candidate:7p 1 udp 1694498815 203.0.113.7 8555 typ srflx",
			wantOK:    true,
		},
		{
			name:      "foundation at maximum length",
			candidate: "candidate:" + longFoundation + " 1 udp 1694498815 203.0.113.7 51234 typ srflx raddr 192.168.101.2 rport 51234",
			want:      "candidate:" + longFoundation[:31] + "p 1 udp 1694498815 203.0.113.7 8555 typ srflx raddr 192.168.101.2 rport 8555",
			wantOK:    true,
		},
		{name: "srflx already on mux port", candidate: "candidate:7 1 udp 1694498815 203.0.113.7 8555 typ srflx raddr 192.168.101.2 rport 8555"},
		{name: "host", candidate: "candidate:1 1 udp 2130706431 192.168.101.2 8555 typ host"},
		{name: "relay", candidate: "candidate:2 1 udp 16777215 198.51.100.9 61000 typ relay raddr 203.0.113.7 rport 51234"},
		{name: "prflx", candidate: "candidate:3 1 udp 1853824767 203.0.113.7 51234 typ prflx raddr 192.168.101.2 rport 8555"},
		{name: "tcp", candidate: "candidate:4 1 tcp 1694498815 203.0.113.7 51234 typ srflx raddr 192.168.101.2 rport 51234 tcptype passive"},
		{name: "ipv6", candidate: "candidate:5 1 udp 1694498815 2001:db8::7 51234 typ srflx raddr :: rport 51234"},
		{name: "ipv4-mapped ipv6", candidate: "candidate:5 1 udp 1694498815 ::ffff:203.0.113.7 51234 typ srflx"},
		{name: "mdns", candidate: "candidate:6 1 udp 1694498815 3c1f8b2e.local 51234 typ srflx"},
		{name: "port not a number", candidate: "candidate:7 1 udp 1694498815 203.0.113.7 abc typ srflx"},
		{name: "port out of range", candidate: "candidate:7 1 udp 1694498815 203.0.113.7 70000 typ srflx"},
		{name: "missing typ keyword", candidate: "candidate:7 1 udp 1694498815 203.0.113.7 51234 kind srflx"},
		{name: "truncated", candidate: "candidate:7 1 udp"},
		{name: "empty", candidate: ""},
		{name: "end of candidates", candidate: "end-of-candidates"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := publicMuxCandidate(tt.candidate)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("publicMuxCandidate(%q) = %q, %v; want %q, %v", tt.candidate, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestPublicMuxAnnouncerOncePerAddress(t *testing.T) {
	var announcer publicMuxAnnouncer

	first := "candidate:1 1 udp 1694498815 203.0.113.7 51234 typ srflx raddr 192.168.101.2 rport 51234"
	sameAddress := "candidate:2 1 udp 1694498815 203.0.113.7 40000 typ srflx raddr 192.168.101.2 rport 40000"
	otherAddress := "candidate:3 1 udp 1694498815 198.51.100.4 40000 typ srflx raddr 192.168.101.2 rport 40000"
	host := "candidate:4 1 udp 2130706431 192.168.101.2 8555 typ host"

	if _, ok := announcer.derive(host); ok {
		t.Fatal("host candidate was announced")
	}

	if got, ok := announcer.derive(first); !ok || got != "candidate:1p 1 udp 1694498815 203.0.113.7 8555 typ srflx raddr 192.168.101.2 rport 8555" {
		t.Fatalf("first srflx = %q, %v", got, ok)
	}

	if got, ok := announcer.derive(sameAddress); ok {
		t.Fatalf("second srflx on the same address was announced: %q", got)
	}

	if got, ok := announcer.derive(otherAddress); !ok || got != "candidate:3p 1 udp 1694498815 198.51.100.4 8555 typ srflx raddr 192.168.101.2 rport 8555" {
		t.Fatalf("srflx on another address = %q, %v", got, ok)
	}
}

func TestWithPublicMuxCandidates(t *testing.T) {
	const (
		host    = "a=candidate:1 1 udp 2130706431 192.168.101.2 8555 typ host"
		srflx   = "a=candidate:2 1 udp 1694498815 203.0.113.7 51234 typ srflx raddr 0.0.0.0 rport 51234"
		srflx2  = "a=candidate:3 1 udp 1694498815 203.0.113.7 40000 typ srflx raddr 0.0.0.0 rport 40000"
		derived = "a=candidate:2p 1 udp 1694498815 203.0.113.7 8555 typ srflx raddr 0.0.0.0 rport 8555"
	)

	header := []string{"v=0", "o=- 1 2 IN IP4 0.0.0.0", "s=-", "t=0 0"}
	video := []string{"m=video 9 UDP/TLS/RTP/SAVPF 96", "c=IN IP4 0.0.0.0", "a=mid:0"}
	audio := []string{"m=audio 9 UDP/TLS/RTP/SAVPF 111", "c=IN IP4 0.0.0.0", "a=mid:1"}

	join := func(eol string, parts ...[]string) string {
		var lines []string
		for _, part := range parts {
			lines = append(lines, part...)
		}

		return strings.Join(lines, eol) + eol
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no srflx is unchanged",
			in:   join("\n", header, video, []string{host, "a=end-of-candidates"}),
			want: join("\n", header, video, []string{host, "a=end-of-candidates"}),
		},
		{
			name: "attribute without the candidate prefix is unchanged",
			in:   join("\n", header, video, []string{"a=1 1 udp 1694498815 203.0.113.7 51234 typ srflx"}),
			want: join("\n", header, video, []string{"a=1 1 udp 1694498815 203.0.113.7 51234 typ srflx"}),
		},
		{
			name: "one srflx",
			in:   join("\n", header, video, []string{host, srflx, "a=end-of-candidates"}),
			want: join("\n", header, video, []string{host, srflx, derived, "a=end-of-candidates"}),
		},
		{
			name: "same address twice in one section",
			in:   join("\n", header, video, []string{srflx, srflx2}),
			want: join("\n", header, video, []string{srflx, derived, srflx2}),
		},
		{
			name: "every media section gets its own",
			in:   join("\n", header, video, []string{srflx}, audio, []string{srflx}),
			want: join("\n", header, video, []string{srflx, derived}, audio, []string{srflx, derived}),
		},
		{
			name: "crlf endings are kept",
			in:   join("\r\n", header, video, []string{srflx}),
			want: join("\r\n", header, video, []string{srflx, derived}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withPublicMuxCandidates(tt.in); got != tt.want {
				t.Fatalf("withPublicMuxCandidates() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

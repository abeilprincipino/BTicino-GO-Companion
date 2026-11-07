package media

import (
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestDescribeAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		address, family, scope string
	}{
		{address: "192.168.1.20", family: "ipv4", scope: "private"},
		{address: "10.0.0.1", family: "ipv4", scope: "private"},
		{address: "100.72.10.1", family: "ipv4", scope: "cgnat"},
		{address: "8.8.8.8", family: "ipv4", scope: "public"},
		{address: "127.0.0.1", family: "ipv4", scope: "loopback"},
		{address: "169.254.3.4", family: "ipv4", scope: "link-local"},
		{address: "0.0.0.0", family: "ipv4", scope: "unspecified"},
		{address: "::ffff:8.8.4.4", family: "ipv4", scope: "public"},
		{address: "2a01:4f8::1", family: "ipv6", scope: "public"},
		{address: "fd00::1", family: "ipv6", scope: "private"},
		{address: "fe80::1", family: "ipv6", scope: "link-local"},
		{address: "a1b2c3.local", family: "mdns", scope: "mdns"},
		{address: "not-an-ip", family: "unknown", scope: "unknown"},
	}

	for _, test := range tests {
		family, scope := describeAddress(test.address)
		if family != test.family || scope != test.scope {
			t.Errorf("describeAddress(%q) = %q, %q, want %q, %q", test.address, family, scope, test.family, test.scope)
		}
	}
}

func TestDescribeCandidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		candidate string
		want      candidateSummary
	}{
		{
			candidate: "candidate:1 1 UDP 2130706431 192.168.1.2 12345 typ host",
			want:      candidateSummary{typ: "host", protocol: "udp", family: "ipv4", scope: "private", port: "12345"},
		},
		{
			candidate: "candidate:2 1 udp 2130706431 a1b2.local 12345 typ host generation 0",
			want:      candidateSummary{typ: "host", protocol: "udp", family: "mdns", scope: "mdns", port: "12345"},
		},
		{
			candidate: "candidate:3 1 udp 1686052607 2a01:4f8::1 40000 typ srflx raddr :: rport 0",
			want:      candidateSummary{typ: "srflx", protocol: "udp", family: "ipv6", scope: "public", port: "40000"},
		},
		{
			candidate: "3 1 udp 1 203.0.113.9 8555 typ srflx raddr 0.0.0.0 rport 8555",
			want:      candidateSummary{typ: "srflx", protocol: "udp", family: "ipv4", scope: "public", port: "8555"},
		},
		{candidate: "candidate:4 1"},
	}

	for _, test := range tests {
		if got := describeCandidate(test.candidate); got != test.want {
			t.Errorf("describeCandidate(%q) = %+v, want %+v", test.candidate, got, test.want)
		}
	}

	if !describeCandidate(tests[1].candidate).mdns() || describeCandidate(tests[0].candidate).mdns() {
		t.Error("mdns() does not follow the address family")
	}
}

func TestCandidatePairSummaries(t *testing.T) {
	t.Parallel()

	report := webrtc.StatsReport{
		"local-host": webrtc.ICECandidateStats{
			ID: "local-host", Type: webrtc.StatsTypeLocalCandidate,
			IP: "192.168.1.2", Port: 8555, Protocol: "udp", CandidateType: webrtc.ICECandidateTypeHost,
		},
		"remote-prflx": webrtc.ICECandidateStats{
			ID: "remote-prflx", Type: webrtc.StatsTypeRemoteCandidate,
			IP: "100.80.1.1", Port: 40000, Protocol: "udp", CandidateType: webrtc.ICECandidateTypePrflx,
		},
		"remote-v6": webrtc.ICECandidateStats{
			ID: "remote-v6", Type: webrtc.StatsTypeRemoteCandidate,
			IP: "2a01:4f8::1", Port: 50000, Protocol: "udp", CandidateType: webrtc.ICECandidateTypeSrflx,
		},
		"pair-b": webrtc.ICECandidatePairStats{
			ID: "pair-b", Type: webrtc.StatsTypeCandidatePair,
			LocalCandidateID: "local-host", RemoteCandidateID: "remote-v6",
			State: webrtc.StatsICECandidatePairStateFailed, RequestsSent: 7,
		},
		"pair-a": webrtc.ICECandidatePairStats{
			ID: "pair-a", Type: webrtc.StatsTypeCandidatePair,
			LocalCandidateID: "local-host", RemoteCandidateID: "remote-prflx",
			State: webrtc.StatsICECandidatePairStateSucceeded, Nominated: true,
			RequestsSent: 3, RequestsReceived: 4, ResponsesSent: 4, ResponsesReceived: 3,
		},
	}

	got := candidatePairSummaries(report)
	if len(got) != 2 {
		t.Fatalf("candidatePairSummaries() returned %d pairs, want 2", len(got))
	}

	want := candidatePairSummary{
		id: "pair-a", state: "succeeded", nominated: true,
		local:        candidateSummary{typ: "host", protocol: "udp", family: "ipv4", scope: "private", port: "8555"},
		remote:       candidateSummary{typ: "prflx", protocol: "udp", family: "ipv4", scope: "cgnat", port: "40000"},
		requestsSent: 3, requestsReceived: 4, responsesSent: 4, responsesReceived: 3,
	}
	if got[0] != want {
		t.Errorf("first pair = %+v, want %+v", got[0], want)
	}

	if got[1].id != "pair-b" || got[1].state != "failed" || got[1].remote.family != "ipv6" || got[1].requestsSent != 7 {
		t.Errorf("second pair = %+v", got[1])
	}
}

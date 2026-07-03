package webrtc

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"bticino-go-companion/internal/config"
)

func newServiceForTest(t *testing.T) *Service {
	t.Helper()
	origPort := webrtcICEPort
	origPreferred := preferredOutboundInterface
	webrtcICEPort = 0
	preferredOutboundInterface = func() (net.Interface, net.IP, error) {
		return net.Interface{Name: "lo"}, net.ParseIP("127.0.0.1"), nil
	}
	t.Cleanup(func() {
		webrtcICEPort = origPort
		preferredOutboundInterface = origPreferred
	})
	svc, err := New(nil, nil, nil, config.Config{})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func TestBuildRTCConfiguration(t *testing.T) {
	if got := buildRTCConfiguration(config.Config{}); len(got.ICEServers) != 0 {
		t.Fatalf("empty config must yield zero-value Configuration, got %+v", got)
	}

	one := buildRTCConfiguration(config.Config{
		WebRTCICEServers: []string{"stun:stun.l.google.com:19302"},
	})
	if len(one.ICEServers) != 1 || len(one.ICEServers[0].URLs) != 1 ||
		one.ICEServers[0].URLs[0] != "stun:stun.l.google.com:19302" {
		t.Fatalf("unexpected single-server configuration: %+v", one)
	}

	two := buildRTCConfiguration(config.Config{
		WebRTCICEServers: []string{"stun:a.example:3478", "   ", "stun:b.example:3478"},
	})
	if len(two.ICEServers) != 2 {
		t.Fatalf("expected 2 ice servers (blank dropped), got %+v", two.ICEServers)
	}
	if two.ICEServers[0].URLs[0] != "stun:a.example:3478" || two.ICEServers[1].URLs[0] != "stun:b.example:3478" {
		t.Fatalf("unexpected ice server ordering/content: %+v", two.ICEServers)
	}
}

func TestAddCandidateQueuesUnknownSession(t *testing.T) {
	svc := newServiceForTest(t)

	candidate := Candidate{
		Candidate: "candidate:1 1 UDP 2122260223 10.0.0.5 54321 typ host",
	}
	if err := svc.AddCandidate("session-1", candidate); err != nil {
		t.Fatalf("add candidate: %v", err)
	}

	svc.mu.RLock()
	batch, ok := svc.pendingCandidates["session-1"]
	svc.mu.RUnlock()
	if !ok {
		t.Fatalf("expected pending candidates for session")
	}
	if len(batch.candidates) != 1 {
		t.Fatalf("expected 1 pending candidate, got %d", len(batch.candidates))
	}
	if batch.candidates[0].Candidate != candidate.Candidate {
		t.Fatalf("unexpected candidate content: %s", batch.candidates[0].Candidate)
	}
}

func TestAddCandidateRejectsEmptyCandidate(t *testing.T) {
	svc := newServiceForTest(t)
	if err := svc.AddCandidate("session-1", Candidate{}); err != ErrCandidateRequired {
		t.Fatalf("expected ErrCandidateRequired, got %v", err)
	}
}

func TestCloseUnknownSessionClearsPendingCandidates(t *testing.T) {
	svc := newServiceForTest(t)
	_ = svc.AddCandidate("session-1", Candidate{Candidate: "candidate:1 1 UDP 2122260223 10.0.0.5 54321 typ host"})

	if err := svc.Close("session-1"); err != nil {
		t.Fatalf("close: %v", err)
	}

	svc.mu.RLock()
	_, ok := svc.pendingCandidates["session-1"]
	svc.mu.RUnlock()
	if ok {
		t.Fatalf("expected pending candidates to be cleared on close")
	}
}

func TestPendingCandidatePrune(t *testing.T) {
	svc := newServiceForTest(t)

	svc.mu.Lock()
	svc.pendingCandidates["expired"] = pendingCandidateBatch{
		candidates: []webrtc.ICECandidateInit{{Candidate: "candidate:expired 1 UDP 1 10.0.0.1 1 typ host"}},
		updatedAt:  time.Now().Add(-(pendingCandidateTTL + time.Second)),
	}
	svc.mu.Unlock()

	_ = svc.AddCandidate("fresh", Candidate{Candidate: "candidate:1 1 UDP 2122260223 10.0.0.5 54321 typ host"})

	svc.mu.RLock()
	_, expiredExists := svc.pendingCandidates["expired"]
	_, freshExists := svc.pendingCandidates["fresh"]
	svc.mu.RUnlock()
	if expiredExists {
		t.Fatalf("expected expired pending candidates to be pruned")
	}
	if !freshExists {
		t.Fatalf("expected fresh pending candidates to remain")
	}
}

func TestCanonicalizeSDP(t *testing.T) {
	raw := "v=0\n" +
		"o=- 1 2 IN IP4 127.0.0.1\n" +
		"s=-\n" +
		"t=0 0\n"

	got := canonicalizeSDP(raw)
	if got == "" {
		t.Fatalf("expected canonicalized sdp")
	}
	if got[len(got)-2:] != "\r\n" {
		t.Fatalf("expected trailing CRLF, got %q", got)
	}
	if !strings.Contains(got, "\r\no=- 1 2 IN IP4 127.0.0.1\r\n") {
		t.Fatalf("expected normalized CRLF lines, got %q", got)
	}
}

func TestAnswerDirectionFromOfferAudio(t *testing.T) {
	tests := []struct {
		name    string
		offer   string
		wantDir webrtc.RTPTransceiverDirection
	}{
		{
			name: "audio recvonly maps to sendonly",
			offer: "v=0\r\n" +
				"o=- 1 1 IN IP4 127.0.0.1\r\n" +
				"s=-\r\n" +
				"t=0 0\r\n" +
				"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
				"a=recvonly\r\n",
			wantDir: webrtc.RTPTransceiverDirectionSendonly,
		},
		{
			name: "audio sendrecv maps to sendrecv",
			offer: "v=0\r\n" +
				"o=- 1 1 IN IP4 127.0.0.1\r\n" +
				"s=-\r\n" +
				"t=0 0\r\n" +
				"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
				"a=sendrecv\r\n",
			wantDir: webrtc.RTPTransceiverDirectionSendrecv,
		},
		{
			name: "audio sendonly maps to recvonly",
			offer: "v=0\r\n" +
				"o=- 1 1 IN IP4 127.0.0.1\r\n" +
				"s=-\r\n" +
				"t=0 0\r\n" +
				"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
				"a=sendonly\r\n",
			wantDir: webrtc.RTPTransceiverDirectionRecvonly,
		},
		{
			name: "audio inherits session direction",
			offer: "v=0\r\n" +
				"o=- 1 1 IN IP4 127.0.0.1\r\n" +
				"s=-\r\n" +
				"t=0 0\r\n" +
				"a=recvonly\r\n" +
				"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n",
			wantDir: webrtc.RTPTransceiverDirectionSendonly,
		},
		{
			name: "no audio mline maps inactive",
			offer: "v=0\r\n" +
				"o=- 1 1 IN IP4 127.0.0.1\r\n" +
				"s=-\r\n" +
				"t=0 0\r\n" +
				"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
				"a=recvonly\r\n",
			wantDir: webrtc.RTPTransceiverDirectionInactive,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := answerDirectionFromOfferAudio(tc.offer)
			if got != tc.wantDir {
				t.Fatalf("answerDirectionFromOfferAudio()=%s want=%s", got.String(), tc.wantDir.String())
			}
		})
	}
}

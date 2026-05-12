package sipprotocol

import (
	"strings"
	"testing"
)

func TestBuildOfferIncludesDevAddr(t *testing.T) {
	offer := BuildOffer(SDPConfig{Host: "127.0.0.1", AudioPort: 5000, VideoPort: 5007, IncludeDevAddr: true, DevAddr: "21"})
	if !strings.Contains(offer, "a=DEVADDR:21") {
		t.Fatalf("expected DEVADDR in offer: %s", offer)
	}
}

func TestBuildOfferOmitsEmptyDevAddr(t *testing.T) {
	offer := BuildOffer(SDPConfig{Host: "127.0.0.1", AudioPort: 5000, VideoPort: 5007, IncludeDevAddr: true})
	if strings.Contains(offer, "a=DEVADDR:") {
		t.Fatalf("expected no DEVADDR when empty: %s", offer)
	}
}

func TestBuildAnswerContainsSpeexAndH264(t *testing.T) {
	answer := BuildAnswer(SDPConfig{})
	if !strings.Contains(answer, "speex/8000") || !strings.Contains(answer, "H264/90000") {
		t.Fatalf("unexpected answer: %s", answer)
	}
	if !strings.Contains(answer, "m=audio 65000 RTP/SAVP 110") {
		t.Fatalf("expected default audio port 65000 in answer: %s", answer)
	}
	if !strings.Contains(answer, "m=video 65002 RTP/SAVP 96") {
		t.Fatalf("expected default video port 65002 in answer: %s", answer)
	}
}

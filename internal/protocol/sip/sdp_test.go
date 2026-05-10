package sip

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

func TestBuildAnswerContainsSpeexAndH264(t *testing.T) {
	answer := BuildAnswer(SDPConfig{})
	if !strings.Contains(answer, "speex/8000") || !strings.Contains(answer, "H264/90000") {
		t.Fatalf("unexpected answer: %s", answer)
	}
}

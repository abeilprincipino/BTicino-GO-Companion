package sip

import (
	"strings"
	"testing"
)

func TestBuildOfferUsesIngestPortsAndDevAddr(t *testing.T) {
	offer := BuildOffer("192.0.2.10", "21")

	for _, line := range []string{
		"m=audio 5000 RTP/SAVP 110",
		"m=video 5007 RTP/SAVP 96",
		"a=DEVADDR:21",
	} {
		if !strings.Contains(offer, line) {
			t.Fatalf("offer does not contain %q: %s", line, offer)
		}
	}
}

func TestBuildAnswerUsesIngestPorts(t *testing.T) {
	answer := BuildAnswer("0.0.0.0")

	for _, line := range []string{
		"o=companion 3747 461 IN IP4 127.0.0.1",
		"m=audio 5000 RTP/SAVP 110",
		"m=video 5007 RTP/SAVP 96",
	} {
		if !strings.Contains(answer, line) {
			t.Fatalf("answer does not contain %q: %s", line, answer)
		}
	}
}

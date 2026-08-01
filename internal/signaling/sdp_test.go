package signaling

import (
	"strings"
	"testing"
)

func TestBuildOfferUsesIngestPortsAndDevAddr(t *testing.T) {
	t.Parallel()

	offer := BuildOffer("192.0.2.10", "21")

	for _, line := range []string{
		"m=audio 65000 RTP/SAVP 110",
		"m=video 65002 RTP/SAVP 96",
		"a=DEVADDR:21",
	} {
		if !strings.Contains(offer, line) {
			t.Fatalf("offer does not contain %q: %s", line, offer)
		}
	}
}

func TestBuildAnswerUsesDummySIPPorts(t *testing.T) {
	t.Parallel()

	answer := BuildAnswer("0.0.0.0", "")

	for _, line := range []string{
		"o=companion 3747 461 IN IP4 127.0.0.1",
		"m=audio 65000 RTP/SAVP 110",
		"m=video 65002 RTP/SAVP 96",
	} {
		if !strings.Contains(answer, line) {
			t.Fatalf("answer does not contain %q: %s", line, answer)
		}
	}
}

func TestBuildAnswerIncludesDevAddr(t *testing.T) {
	t.Parallel()

	answer := BuildAnswer("192.0.2.10", "21")

	if !strings.Contains(answer, "a=DEVADDR:21") {
		t.Fatalf("answer missing DEVADDR: %s", answer)
	}

	devAddrIndex := strings.Index(answer, "a=DEVADDR:21")
	audioIndex := strings.Index(answer, "m=audio")

	if devAddrIndex < 0 || audioIndex < 0 || devAddrIndex > audioIndex {
		t.Fatalf("DEVADDR must precede the media sections: %s", answer)
	}
}

func TestBuildAnswerOmitsEmptyDevAddr(t *testing.T) {
	t.Parallel()

	answer := BuildAnswer("192.0.2.10", "")

	if strings.Contains(answer, "DEVADDR") {
		t.Fatalf("answer must not contain DEVADDR when devaddr is empty: %s", answer)
	}
}

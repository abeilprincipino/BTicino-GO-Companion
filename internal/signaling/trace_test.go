package signaling

import "testing"

func TestSIPSummaryReadsStartLineCallIDAndCSeq(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		message       string
		wantStartLine string
		wantCallID    string
		wantCSeq      string
	}{
		{
			name: "request",
			message: "INVITE sip:companion@127.0.0.1:5070 SIP/2.0\nVia: SIP/2.0/UDP 127.0.0.1:5060\n" +
				"Call-ID: abc@127.0.0.1\nCSeq: 20 INVITE\nContent-Length: 0\n\nv=0\no=- Call-ID: not-a-header\n",
			wantStartLine: "INVITE sip:companion@127.0.0.1:5070 SIP/2.0",
			wantCallID:    "abc@127.0.0.1",
			wantCSeq:      "20 INVITE",
		},
		{
			name:          "response with compact call id",
			message:       "SIP/2.0 603 Decline\ni: xyz\ncseq: 20 INVITE\n\n",
			wantStartLine: "SIP/2.0 603 Decline",
			wantCallID:    "xyz",
			wantCSeq:      "20 INVITE",
		},
		{name: "start line only", message: "OPTIONS sip:x SIP/2.0", wantStartLine: "OPTIONS sip:x SIP/2.0"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			startLine, callID, cseq := sipSummary(test.message)
			if startLine != test.wantStartLine || callID != test.wantCallID || cseq != test.wantCSeq {
				t.Fatalf("sipSummary() = %q, %q, %q, want %q, %q, %q",
					startLine, callID, cseq, test.wantStartLine, test.wantCallID, test.wantCSeq)
			}
		})
	}
}

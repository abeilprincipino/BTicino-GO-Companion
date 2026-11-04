package signaling

import (
	"bytes"
	"context"
	"log/slog"
	"strings"

	"github.com/emiago/sipgo/sip"
)

// EnableSIPTrace logs every SIP message the companion reads or writes, on every
// transport, at debug. The log level can change at runtime, so the hook stays
// installed and each message checks the level.
//
// sipgo keeps the switch and the tracer in package globals that its transports
// read without a lock, so this must run once, before the first dialer exists.
func EnableSIPTrace(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	sip.SIPDebugTracer(sipTracer{logger: logger.With("component", "sip.trace")})
	sip.SIPDebug = true
}

// sipTracer implements sip.SIPTracer.
type sipTracer struct {
	logger *slog.Logger
}

func (t sipTracer) SIPTraceRead(transport, laddr, raddr string, sipmsg []byte) {
	t.trace("in", transport, laddr, raddr, sipmsg)
}

func (t sipTracer) SIPTraceWrite(transport, laddr, raddr string, sipmsg []byte) {
	t.trace("out", transport, laddr, raddr, sipmsg)
}

func (t sipTracer) trace(direction, transport, laddr, raddr string, sipmsg []byte) {
	ctx := context.Background()
	if !t.logger.Enabled(ctx, slog.LevelDebug) {
		return
	}

	// A TCP read can be empty, or a keepalive CRLF.
	if len(bytes.TrimSpace(sipmsg)) == 0 {
		return
	}

	message := strings.ReplaceAll(string(sipmsg), "\r\n", "\n")
	startLine, callID, cseq := sipSummary(message)

	t.logger.DebugContext(ctx, "sip message",
		"direction", direction,
		"transport", transport,
		"local", laddr,
		"remote", raddr,
		"start_line", startLine,
		"call_id", callID,
		"cseq", cseq,
		"message", strings.TrimRight(message, "\n"),
	)
}

// sipSummary reads the start line, the Call-ID and the CSeq of a SIP message,
// so the trace can be followed without reading each message whole. Compact
// header names (i for Call-ID) are recognised too.
func sipSummary(message string) (startLine, callID, cseq string) {
	head, _, _ := strings.Cut(message, "\n\n")
	lines := strings.Split(head, "\n")
	startLine = strings.TrimSpace(lines[0])

	for _, line := range lines[1:] {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}

		switch strings.ToLower(strings.TrimSpace(name)) {
		case "call-id", "i":
			callID = strings.TrimSpace(value)
		case "cseq":
			cseq = strings.TrimSpace(value)
		}
	}

	return startLine, callID, cseq
}

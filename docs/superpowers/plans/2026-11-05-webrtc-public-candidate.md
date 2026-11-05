# WebRTC Public Candidate on Port 8555 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Announce `<public IP>:8555` next to every server-reflexive ICE candidate, so a router forward of UDP 8555 lets a phone on a mobile network reach the intercom, and document the remote access paths in the integration README.

**Architecture:** A pure string function derives the extra candidate from pion's server-reflexive one; a small per-session announcer de-duplicates it by public address. The companion sends it through the existing trickle callback and, for clients that do not trickle, inserts it into the answer SDP. pion answers the incoming checks on the existing UDP mux as a peer-reflexive pair, so no pion configuration changes.

**Tech Stack:** Go 1.26, pion/webrtc v4.2.16, pion/ice v4.2.7; Markdown README in the separate `BTicinoGO-Integration` repo.

**Spec:** [docs/superpowers/specs/2026-11-05-webrtc-public-candidate-design.md](../specs/2026-11-05-webrtc-public-candidate-design.md)

## Global Constraints

- Always on, no config key (no new `config.yaml` field).
- External port must be 8555 (`webrtcICEPort`). UDP and IPv4 only.
- The public IP is never logged.
- The new code never fails a session: anything it cannot parse yields no extra candidate.
- `internal/media` does not compile on Windows. Do not run its tests locally and do not cross-compile test binaries for WSL. Verify with `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/` and leave `go test` to CI; say in the PR that the tests were not run locally.
- Logging is `log/slog` key-value pairs, like the surrounding code. Lint config is `.golangci.yml` (`wsl_v5`, `godot`: comments end with a period).
- Line endings LF.
- Companion branch: `feat/webrtc-public-candidate` (already created from `origin/develop`, holds the spec). Integration branch: create `docs/remote-access` from `origin/develop` in `D:\Progetti\BTicinoGO-Integration`.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## File Structure

| File | Repo | Responsibility |
| --- | --- | --- |
| `internal/media/webrtc_public_candidate.go` (create) | companion | `publicMuxCandidate`, `publicMuxAnnouncer`, `withPublicMuxCandidates`: pure string logic, no pion types |
| `internal/media/webrtc_public_candidate_test.go` (create) | companion | Table tests for the three units |
| `internal/media/webrtc.go` (modify) | companion | Call the announcer from `OnICECandidate` and the SDP transform from `createOfferAnswer` |
| `README.md` (modify) | integration | New "Remote access to the video" section, TOC entry, troubleshooting fix |

---

### Task 1: Candidate derivation (pure functions)

**Files:**
- Create: `internal/media/webrtc_public_candidate.go`
- Test: `internal/media/webrtc_public_candidate_test.go`

**Interfaces:**
- Consumes: `webrtcICEPort` (untyped const `8555`, `internal/media/webrtc.go:25`).
- Produces:
  - `func publicMuxCandidate(candidate string) (string, bool)`
  - `type publicMuxAnnouncer struct` (zero value ready to use) with `func (a *publicMuxAnnouncer) derive(candidate string) (string, bool)`
  - `func withPublicMuxCandidates(sdp string) string`

- [ ] **Step 1: Write the failing tests**

Create `internal/media/webrtc_public_candidate_test.go`:

```go
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
```

- [ ] **Step 2: Verify the tests fail to build**

Run: `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/`
Expected: FAIL with `undefined: publicMuxCandidate`, `undefined: publicMuxAnnouncer`, `undefined: withPublicMuxCandidates`.

- [ ] **Step 3: Write the implementation**

Create `internal/media/webrtc_public_candidate.go`:

```go
package media

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const (
	iceCandidatePrefix = "candidate:"
	// maxICEFoundationLength is the foundation limit of the ICE grammar (RFC 8839).
	maxICEFoundationLength = 32
	publicMuxFoundationTag = "p"
)

// Field positions of an ICE candidate attribute: foundation, component,
// transport, priority, address, port, "typ", type, then name-value pairs.
const (
	candidateFoundationField = 0
	candidateTransportField  = 2
	candidateAddressField    = 4
	candidatePortField       = 5
	candidateTypKeywordField = 6
	candidateTypeField       = 7
	candidateMinFields       = 8
)

// publicMuxCandidate derives the candidate that announces the ICE mux port on
// the public address of a server-reflexive candidate. A home router that
// forwards UDP 8555 to the intercom then lets a peer off the LAN reach the mux,
// where pion answers its checks as a peer-reflexive pair. It returns false for
// any other candidate, including ones it cannot parse.
func publicMuxCandidate(candidate string) (string, bool) {
	body, hasPrefix := strings.CutPrefix(strings.TrimSpace(candidate), iceCandidatePrefix)
	fields := strings.Fields(body)

	if !isPublicMuxSource(fields) {
		return "", false
	}

	muxPort := strconv.Itoa(webrtcICEPort)
	derived := slices.Clone(fields)
	derived[candidateFoundationField] = publicMuxFoundation(fields[candidateFoundationField])
	derived[candidatePortField] = muxPort

	for i := candidateMinFields; i+1 < len(derived); i += 2 {
		if derived[i] == "rport" {
			derived[i+1] = muxPort
		}
	}

	result := strings.Join(derived, " ")

	if hasPrefix {
		return iceCandidatePrefix + result, true
	}

	return result, true
}

// isPublicMuxSource reports whether fields are a UDP IPv4 server-reflexive
// candidate on a port other than the mux one.
func isPublicMuxSource(fields []string) bool {
	if len(fields) < candidateMinFields ||
		!strings.EqualFold(fields[candidateTransportField], "udp") ||
		fields[candidateTypKeywordField] != "typ" ||
		fields[candidateTypeField] != "srflx" {
		return false
	}

	address, err := netip.ParseAddr(fields[candidateAddressField])
	if err != nil || !address.Is4() {
		return false
	}

	port, err := strconv.ParseUint(fields[candidatePortField], 10, 16)

	return err == nil && port != webrtcICEPort
}

// publicMuxFoundation gives the derived candidate its own foundation, so the
// peer does not pair it as a copy of the random-port candidate.
func publicMuxFoundation(foundation string) string {
	if len(foundation) < maxICEFoundationLength {
		return foundation + publicMuxFoundationTag
	}

	return foundation[:maxICEFoundationLength-1] + publicMuxFoundationTag
}

// publicMuxAnnouncer derives the mux candidate once per public address. The
// zero value is ready to use.
type publicMuxAnnouncer struct {
	mu        sync.Mutex
	announced map[string]struct{}
}

func (a *publicMuxAnnouncer) derive(candidate string) (string, bool) {
	derived, ok := publicMuxCandidate(candidate)
	if !ok {
		return "", false
	}

	body, _ := strings.CutPrefix(derived, iceCandidatePrefix)
	address := strings.Fields(body)[candidateAddressField]

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, done := a.announced[address]; done {
		return "", false
	}

	if a.announced == nil {
		a.announced = make(map[string]struct{})
	}

	a.announced[address] = struct{}{}

	return derived, true
}

// withPublicMuxCandidates adds, after each server-reflexive a=candidate line,
// the candidate derived by publicMuxCandidate, once per public address and
// media section. It serves the answers of clients that do not trickle.
func withPublicMuxCandidates(sdp string) string {
	eol := "\n"

	if strings.Contains(sdp, "\r\n") {
		eol = "\r\n"
	}

	lines := strings.Split(sdp, eol)
	out := make([]string, 0, len(lines))
	section := &publicMuxAnnouncer{}

	for _, line := range lines {
		if strings.HasPrefix(line, "m=") {
			section = &publicMuxAnnouncer{}
		}

		out = append(out, line)

		attribute, isAttribute := strings.CutPrefix(line, "a=")
		if !isAttribute {
			continue
		}

		if derived, ok := section.derive(attribute); ok {
			out = append(out, "a="+derived)
		}
	}

	return strings.Join(out, eol)
}
```

- [ ] **Step 4: Verify it builds for the target**

Run: `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/`
Expected: no output, exit 0.

If Docker is running (`docker version` shows a Server section), also run `docker build --target test .` and expect it to finish. Otherwise the tests run in CI on push.

- [ ] **Step 5: Commit**

```bash
git add internal/media/webrtc_public_candidate.go internal/media/webrtc_public_candidate_test.go
git commit -F - <<'EOF'
feat(media): derive the public candidate on the ICE mux port

publicMuxCandidate turns a server-reflexive candidate into one on
<public IP>:8555, and withPublicMuxCandidates adds it to an answer SDP,
once per address and media section. Not wired in yet.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 2: Announce the candidate in trickle and in the answer

**Files:**
- Modify: `internal/media/webrtc.go` — `newOfferSession` (`OnICECandidate` callback, around lines 363-384) and `createOfferAnswer` (around lines 500-509)

**Interfaces:**
- Consumes: `publicMuxAnnouncer.derive(candidate string) (string, bool)` and `withPublicMuxCandidates(sdp string) string` from Task 1.
- Produces: nothing new for other tasks.

There is no unit test for this wiring: pion only gathers a server-reflexive candidate against a real STUN server. The behaviour is covered by Task 1's tests and verified on the intercom in Task 4.

- [ ] **Step 1: Forward the derived candidate after the trickled one**

In `newOfferSession`, replace the whole `pc.OnICECandidate(...)` block:

```go
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			s.logger.Debug("webrtc local candidate gathered", "session_id", sessionID, "entrypoint_id", entrypointID, "type", candidate.Typ.String(), "protocol", candidate.Protocol.String())
		}

		if trickle == nil {
			return
		}

		if candidate == nil {
			trickle.Candidate(nil)
			return
		}

		candidateJSON := candidate.ToJSON()
		trickle.Candidate(&ICECandidate{
			Candidate:        candidateJSON.Candidate,
			SDPMid:           candidateJSON.SDPMid,
			SDPMLineIndex:    candidateJSON.SDPMLineIndex,
			UsernameFragment: candidateJSON.UsernameFragment,
		})
	})
```

with:

```go
	publicMux := &publicMuxAnnouncer{}

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			s.logger.Debug("webrtc local candidate gathered", "session_id", sessionID, "entrypoint_id", entrypointID, "type", candidate.Typ.String(), "protocol", candidate.Protocol.String())
		}

		if trickle == nil {
			return
		}

		if candidate == nil {
			trickle.Candidate(nil)
			return
		}

		candidateJSON := candidate.ToJSON()
		trickle.Candidate(&ICECandidate{
			Candidate:        candidateJSON.Candidate,
			SDPMid:           candidateJSON.SDPMid,
			SDPMLineIndex:    candidateJSON.SDPMLineIndex,
			UsernameFragment: candidateJSON.UsernameFragment,
		})

		// A router forward of the mux port to the intercom makes this one reachable
		// from outside the LAN; without it, its checks fail and nothing changes.
		if derived, ok := publicMux.derive(candidateJSON.Candidate); ok {
			s.logger.Debug("webrtc public mux candidate announced", "session_id", sessionID, "entrypoint_id", entrypointID, "path", "trickle")
			trickle.Candidate(&ICECandidate{
				Candidate:        derived,
				SDPMid:           candidateJSON.SDPMid,
				SDPMLineIndex:    candidateJSON.SDPMLineIndex,
				UsernameFragment: candidateJSON.UsernameFragment,
			})
		}
	})
```

- [ ] **Step 2: Add it to the answer of clients that do not trickle**

In `createOfferAnswer`, replace:

```go
	s.logNegotiatedMedia(ctx, session)
	s.logger.DebugContext(ctx, "webrtc offer accepted", "session_id", sessionID, "entrypoint_id", entrypointID, "remote_candidates_queued", session.queuedCandidates.Load(), "remote_candidates_applied", session.appliedCandidates.Load())

	return local.SDP, nil
}
```

with:

```go
	// A trickling client already got every derived candidate through the callback.
	answerSDP := local.SDP

	if session.trickle == nil {
		answerSDP = withPublicMuxCandidates(answerSDP)
		if answerSDP != local.SDP {
			s.logger.DebugContext(ctx, "webrtc public mux candidate announced", "session_id", sessionID, "entrypoint_id", entrypointID, "path", "answer")
		}
	}

	s.logNegotiatedMedia(ctx, session)
	s.logger.DebugContext(ctx, "webrtc offer accepted", "session_id", sessionID, "entrypoint_id", entrypointID, "remote_candidates_queued", session.queuedCandidates.Load(), "remote_candidates_applied", session.appliedCandidates.Load())

	return answerSDP, nil
}
```

- [ ] **Step 3: Verify the whole tree for the target**

Run: `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./...`
Expected: no output, exit 0.

Run: `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -buildvcs=false -trimpath -ldflags "-s -w -X bticino-go-companion/internal/system.BuildVersion=v0.0.0-local -X bticino-go-companion/internal/system.BuildGitSHA=$(git rev-parse --short=7 HEAD) -X bticino-go-companion/internal/system.BuildReleaseRepo=" -o dist/companion ./cmd/companion`
Expected: exit 0, `dist/companion` written.

Run: `go test ./internal/signaling/... ./internal/core/...`
Expected: PASS (sanity check of the packages that run on Windows; they are not touched).

- [ ] **Step 4: Commit**

```bash
git add internal/media/webrtc.go
git commit -F - <<'EOF'
feat(media): announce the public candidate on the ICE mux port

Next to each server-reflexive candidate the companion now announces
<public IP>:8555, through trickle or in the answer of a client that does
not trickle. With UDP 8555 forwarded to the intercom, a phone on a mobile
network reaches the mux and pion answers it as a peer-reflexive pair;
without the forward the extra candidate fails and nothing else changes.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 3: Remote access guide in the integration README

**Files:**
- Modify: `D:\Progetti\BTicinoGO-Integration\README.md` — TOC (lines 16-28), new section before `## Troubleshooting` (line 300), troubleshooting bullet at lines 315-316

**Interfaces:**
- Consumes: nothing from the companion tasks. The port-forward path is added in Task 4, after the real test.
- Produces: the `#remote-access-to-the-video` anchor that Task 4 extends.

- [ ] **Step 1: Branch the integration repo**

```bash
cd /d/Progetti/BTicinoGO-Integration
git fetch origin
git switch -c docs/remote-access origin/develop
git branch --unset-upstream
```

Expected: `Switched to a new branch 'docs/remote-access'`. Uncommitted changes in that repo, if any, stop the switch: report them, do not discard them.

- [ ] **Step 2: Add the TOC entry**

Replace:

```markdown
- [Updating](#updating)
- [Troubleshooting](#troubleshooting)
```

with:

```markdown
- [Updating](#updating)
- [Remote access to the video](#remote-access-to-the-video)
- [Troubleshooting](#troubleshooting)
```

- [ ] **Step 3: Add the section before `## Troubleshooting`**

Insert immediately before the line `## Troubleshooting`:

````markdown
## Remote access to the video

Home Assistant remote access (Home Assistant Cloud, a reverse proxy, a Cloudflare Tunnel with a
public hostname) carries the dashboard and the call signaling, not the video. The video is a
WebRTC stream between your phone and the intercom: away from home, if the card rings or the
intercom clicks but the picture stays black, the stream has no path between the two.

Pick one of these paths:

| Path | What you need | Works when your home line is behind CGNAT | Verified with this integration |
| --- | --- | --- | --- |
| [VPN to your home network](#vpn-to-your-home-network) | A VPN app active on the phone | Tailscale: yes. WireGuard: only with a reachable endpoint | Not yet |
| [TURN server](#turn-server) | A TURN server you run or rent | Yes | Not yet |
| Home Assistant Cloud | A Home Assistant Cloud subscription | Yes | Not yet |

If a path works or fails for you, please say so in an issue, with your network setup.

### VPN to your home network

With a VPN the phone is on your home network, so it reaches the intercom on UDP `8555` exactly as
at home.

- **Tailscale:** run Tailscale on a machine in your home network (for example the Home Assistant
  Tailscale add-on) as a subnet router for the intercom's subnet, approve the route in the
  Tailscale admin console, and keep Tailscale on in the phone. It also works when your home line
  has no public IP.
- **WireGuard:** the phone's tunnel must route the intercom's subnet (`AllowedIPs`), and the
  WireGuard server at home needs a reachable UDP port, so a public IP or a forward on your router.

### TURN server

A TURN server relays the stream when no direct path exists. Add it to Home Assistant's
`configuration.yaml`; the integration passes Home Assistant's ICE servers to the companion with
every call:

```yaml
web_rtc:
  ice_servers:
    - url: "turn:turn.example.com:3478"
      username: "user"
      credential: "secret"
```

- Run your own (for example coturn on a small VPS) or use a hosted TURN service.
- The relay carries the whole stream, so count its traffic against your server's or provider's
  allowance.
- The browser receives these credentials to open the stream, so any Home Assistant user who opens
  the card can read them. Use credentials dedicated to this purpose.

### Cloudflare Tunnel and other reverse proxies

A Cloudflare Tunnel with a public hostname (the `cloudflared` add-on or container) and reverse
proxies such as Nginx or Traefik carry HTTP and WebSocket only. They make Home Assistant reachable,
which the video still needs for signaling, but they do not carry the video itself: combine them
with one of the paths above.

### Is my home line behind CGNAT?

Compare the WAN (internet) IP shown in your router's status page with the address shown by a
"what is my IP" website, from a device at home. If they are the same, your line has a public IP.
If they differ, your router is not directly on the internet:

- a WAN IP from `100.64.` to `100.127.` means CGNAT: your provider shares one public IP among
  several customers, and no forward can reach your home;
- a WAN IP starting with `10.`, `172.16.` to `172.31.` or `192.168.` usually means another router
  (often the provider's modem) sits in front of yours: a forward then has to be set on that one
  too, or that one may itself be behind CGNAT.

````

- [ ] **Step 4: Fix the troubleshooting bullet**

Replace:

```markdown
- Confirm TCP `8080` on the intercom is reachable from Home Assistant, and UDP `8555` (WebRTC ICE)
  from the browser's network.
```

with:

```markdown
- Confirm TCP `8080` on the intercom is reachable from Home Assistant, and UDP `8555` (WebRTC ICE)
  from the browser when it is on your home network.
- Away from home, see [Remote access to the video](#remote-access-to-the-video): Home Assistant
  remote access alone does not carry the video.
```

- [ ] **Step 5: Check the anchors**

Run: `grep -n "remote-access-to-the-video\|vpn-to-your-home-network\|turn-server\|^## Remote access\|^### VPN to\|^### TURN server" README.md`
Expected: the three link targets each match a heading (`## Remote access to the video`, `### VPN to your home network`, `### TURN server`), and the TOC and troubleshooting lines link to `#remote-access-to-the-video`.

- [ ] **Step 6: Commit**

```bash
git add README.md
git commit -F - <<'EOF'
docs: explain how to reach the video away from home

Home Assistant remote access carries the signaling, not the WebRTC
media. List the paths that do (VPN, TURN through web_rtc, Home Assistant
Cloud), what a Cloudflare Tunnel does and does not carry, and how to tell
CGNAT. The troubleshooting entry no longer suggests that opening UDP 8555
is enough.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

---

### Task 4: Verify on the intercom and add the port-forward path to the guide

This task needs the user: router access, a phone on a mobile network, and the C100X (`ssh citofono`, 192.168.101.2). Do not start it without them, and stop and report after each case.

**Files:**
- Modify: `D:\Progetti\BTicinoGO-Integration\README.md` — table and new subsection in `## Remote access to the video`

**Interfaces:**
- Consumes: `dist/companion` from Task 2; the section from Task 3.

- [ ] **Step 1: Deploy the build**

```bash
cd /d/Progetti/BTicinoGO
scp dist/companion citofono:/home/bticino/cfg/extra/companion/companion.new
ssh citofono /etc/init.d/companion restart
ssh citofono "sleep 3; tail -n 20 /tmp/companion.log"
```

Expected: the log shows the companion starting with no error. Debug logging must be on to see the `webrtc public mux candidate announced` lines; the pair line is Info.

- [ ] **Step 2: Case 1 — mobile network with the forward**

The user forwards UDP 8555 on the router to 192.168.101.2:8555, turns Wi-Fi off on the phone, and opens the card through the remote Home Assistant URL.

Run: `ssh citofono "grep -E 'public mux candidate announced|candidate pair selected' /tmp/companion.log | tail -n 5"`
Expected: `webrtc public mux candidate announced` for the session, then `webrtc candidate pair selected ... local_type=host remote_type=prflx protocol=udp`; the user confirms the video plays.

- [ ] **Step 3: Case 2 — mobile network without the forward**

The user removes the forward and opens the card again on mobile data.
Expected: the same outcome as before this change for the user's setup (no video without TURN or VPN), and no extra delay before the outcome.

- [ ] **Step 4: Case 3 — LAN**

The user opens the card on home Wi-Fi.
Expected: `webrtc candidate pair selected ... local_type=host remote_type=host` and video plays.

- [ ] **Step 5: Case 4 — TURN**

Only if the user has a TURN server configured in Home Assistant (`web_rtc: ice_servers`) or Home Assistant Cloud: mobile data, no forward.
Expected: `webrtc candidate pair selected` with `local_type=relay` or `remote_type=relay`, and video plays. Update the "Verified" column of the TURN or Home Assistant Cloud row in the guide to the outcome. If no TURN is available, say so and leave the row as "Not yet".

- [ ] **Step 6: Add the port-forward path to the guide (only if case 1 passed)**

In `D:\Progetti\BTicinoGO-Integration\README.md`, add this row at the end of the table in `## Remote access to the video`, using the companion version that ships Task 2 (the beta tag created for it; ask the user):

```markdown
| [Forward UDP 8555 to the intercom](#forward-udp-8555-to-the-intercom) | A public IP on your home line and companion <version> or later | No | Yes, C100X on a mobile network |
```

and insert this subsection right after the `### TURN server` subsection:

```markdown
### Forward UDP 8555 to the intercom

If your home line has a public IP (see [Is my home line behind CGNAT?](#is-my-home-line-behind-cgnat)),
forward UDP port `8555` on your router to the intercom's LAN IP, port `8555`. The external port
must also be `8555`. The companion announces your public IP with that port on every call, and
picks up a new IP by itself when your provider changes it.

This works together with a Cloudflare Tunnel or any other way of reaching Home Assistant: the tunnel
carries the signaling, the forward carries the video.
```

Then commit:

```bash
cd /d/Progetti/BTicinoGO-Integration
git add README.md
git commit -F - <<'EOF'
docs: add the UDP 8555 forward as a remote video path

Tried on a C100X from a mobile network with the companion version that
announces the public candidate on the mux port.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
```

If case 1 failed, do not add the row: report the log lines from Step 2 to the user instead.

- [ ] **Step 7: Hand over for PRs**

Report to the user: the companion commits on `feat/webrtc-public-candidate`, the integration commits on `docs/remote-access`, the outcome of each case, and that `internal/media` tests were not run locally. Do not push or open PRs until the user asks. Companion PRs target `develop` and state the real-intercom test (C100X).

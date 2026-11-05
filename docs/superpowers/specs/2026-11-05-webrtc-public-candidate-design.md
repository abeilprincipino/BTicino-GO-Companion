# WebRTC Public Candidate on Port 8555 — Design

**Date:** 2026-11-05
**Status:** Approved design, not implemented
**Repos:** `BTicinoGO` (companion), `BTicinoGO-Integration` (guide)
**Supersedes the open points of:** [2026-11-04-webrtc-public-candidate-abstract.md](2026-11-04-webrtc-public-candidate-abstract.md)

---

## Goal

A user whose Home Assistant is reachable from outside (for example through a cloudflared tunnel
with a public hostname) and whose home line has a real public IPv4 can watch the intercom from a
mobile network by forwarding UDP 8555 on the router to the intercom. No TURN server, VPN or Home
Assistant Cloud is needed for that case.

A cloudflared public hostname carries HTTP and WebSocket only: the offer reaches the companion
(the intercom "tac"), the UDP media does not. This design gives the media its own path.

## Background

The companion listens for ICE on a single UDP mux on port 8555 (`NewWebRTCService`,
`internal/media/webrtc.go`). Today it announces:

| Candidate | Address | Reachable from a mobile network |
| --- | --- | --- |
| Host | `<private IP>:8555` | No |
| Server-reflexive | `<public IP>:<random port>` | Only with a permissive NAT; a router forward cannot target a random port |

The server-reflexive candidate is gathered on an ephemeral socket because `SetICEUDPMux` covers
host candidates only.

## Approach

Derive one extra candidate from the server-reflexive one at signaling level: same public IP, port
8555. The phone sends its connectivity checks to `<public IP>:8555`, the router forwards them to
the mux, and pion handles them as it already does for any unannounced peer:

- `UDPMuxDefault` routes a STUN packet from an unknown address by the ufrag in its `USERNAME`
  attribute (pion/ice v4.2.7, `udp_mux.go`).
- The agent turns an unknown checking peer into a remote peer-reflexive candidate
  (`agent.go`), and answers from the mux socket, so the router translates the reply back to
  `<public IP>:8555`.

The companion is the answerer, hence the ICE controlled side; the browser nominates the pair.

**Rejected alternative:** pion's NAT 1:1 / address rewrite rules on the `SettingEngine`. They need
the public IP when the API is built, so they would require a separate STUN discovery, a periodic
refresh for dynamic IPs, and an API per offer over the shared mux. The chosen approach reads the
public IP that each session's own gathering already finds, so a dynamic IP needs no refresh: every
new session announces the current address.

## Components

### 1. `publicMuxCandidate` (new file `internal/media/webrtc_public_candidate.go`)

```go
// publicMuxCandidate derives the candidate that announces the ICE mux port on
// the public address of a server-reflexive candidate.
func publicMuxCandidate(candidate string) (string, bool)
```

- Accepts the candidate attribute with or without the `candidate:` prefix, and keeps the same form.
- Returns a candidate only for `udp`, IPv4, `typ srflx`, with a port other than `webrtcICEPort`.
- The derived candidate copies every field and changes three:
  - port → `8555`;
  - `rport` → `8555`, while `raddr` is kept;
  - foundation → the original foundation followed by `p`, so the browser does not group it with
    the random-port candidate. When the original already has 32 characters, its last character
    is replaced by `p` instead, keeping the 32-character limit of the ICE grammar.
- Anything else (host, relay, prflx, TCP, IPv6, mDNS, malformed input) returns `false`. The
  function never fails a session.

### 2. Trickle path

In the `OnICECandidate` callback of `newOfferSession`, after forwarding a candidate to
`trickle.Candidate`, forward its derived candidate too, with the same `sdpMid`,
`sdpMLineIndex` and `usernameFragment`. At most one derived candidate per public IP per session:
the session keeps the set of IPs already announced.

### 3. Non-trickle answer

`createOfferAnswer` returns `local.SDP` with the derived candidates added:

```go
// withPublicMuxCandidates adds, after each server-reflexive a=candidate line,
// the candidate derived by publicMuxCandidate, once per public IP and media section.
func withPublicMuxCandidates(sdp string) string
```

It works line by line, keeps the line endings of the input, and leaves an SDP without
server-reflexive candidates unchanged.

### 4. Logging

- Debug `webrtc public mux candidate announced` with `session_id`, `entrypoint_id` and the path
  (`trickle` or `answer`). The public IP is not logged.
- The existing selected-pair log shows when the forward won: local host candidate on 8555,
  remote peer-reflexive.

## Behaviour

- Always on, no config key. Without a forward, or behind CGNAT at home, the candidate's checks
  fail and the other pairs connect as today. The public IP is already exposed by the existing
  server-reflexive candidate.
- External port must be 8555. UDP only.
- An IP change during a session drops that session, as for any WebRTC peer; the next session
  announces the new IP.
- The intercom firewall already accepts UDP 8555.

## Testing

Unit tests in `internal/media` (they run in CI only: the package does not compile on Windows;
locally verify with `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/`):

- `publicMuxCandidate` table: srflx UDP IPv4 with and without `candidate:` prefix; srflx already
  on 8555; host; relay; prflx; TCP; IPv6; mDNS; malformed; foundation at maximum length.
- `withPublicMuxCandidates`: one srflx line; two media sections; duplicate public IP in one
  section; no srflx; CRLF and LF endings.
- Trickle: a fake `OfferTrickle` receives the derived candidate once after a srflx candidate.
  If driving pion's callback is impractical in a unit test, extract the per-session dedup and
  forwarding into a helper and test that.

Real intercom (the user's C100X, home line with public IPv4, UDP 8555 forwarded):

1. Mobile network, forward active: `webrtc candidate pair selected` shows local host 8555 and
   remote prflx; video plays.
2. Mobile network, forward removed: same behaviour as before the change, no added delay.
3. LAN: the host pair is still selected.
4. TURN configured in Home Assistant: relay still works when the direct path does not.

## Guide (BTicinoGO-Integration README)

New section "Remote access to the video", linked from the troubleshooting section:

- Why it is needed: Home Assistant remote access (Nabu Casa, reverse proxy, cloudflared public
  hostname) carries the page and the signaling, not the WebRTC media.
- A table to choose a path:

  | Path | Needs | Works behind CGNAT at home |
  | --- | --- | --- |
  | VPN to the home LAN (Tailscale, WireGuard) | VPN app active on the phone | Tailscale yes; WireGuard needs a reachable endpoint |
  | TURN via `web_rtc: ice_servers` in Home Assistant | A TURN server (coturn on a VPS or a hosted service) | Yes |
  | Home Assistant Cloud | Subscription | Yes |
  | Forward UDP 8555 to the intercom | Public IPv4 at home, companion version with this feature | No |

- How to tell CGNAT: the router's WAN IP differs from the address shown by a "what is my IP" site.
- cloudflared note: a public hostname tunnel does not carry the video; pick one of the paths above.
- Replace the current troubleshooting line ("UDP 8555 … from the browser's network") with a link
  to the new section.
- Paths not yet tried on a real setup are marked as such. The forward row is added only after
  real-intercom case 1 passes, with the companion version that ships it.

## Out of scope

- A configurable external port, TCP 8555, IPv6.
- An HTTP/WebSocket media fallback (HLS or MSE) for users with no usable path.
- Generating short-lived TURN credentials (for example Cloudflare Realtime TURN) in the integration.

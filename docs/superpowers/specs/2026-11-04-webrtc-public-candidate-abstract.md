# WebRTC Public Candidate on Port 8555 — Abstract

**Date:** 2026-11-04
**Status:** Abstract, deferred: not designed in detail and not implemented
**Repo:** `BTicinoGO` (companion)

---

## Problem

A user on a mobile network cannot get the intercom stream, although the intercom audibly starts it
(the "tac"). That sound only proves that the offer reached the companion through Home Assistant and
that the source started. The media path between phone and intercom is negotiated separately by ICE,
which is the suspected failure. This has not been confirmed with the user's logs.

Forwarding UDP 8555 on the router cannot help today, because the companion never announces that
port on a public address:

| Candidate | Announced address | Why the forward does not help |
| --- | --- | --- |
| Host | `<private IP>:8555` | Not reachable from outside the LAN |
| Server-reflexive | `<public IP>:<random port>` | `SetICEUDPMux` covers host candidates only, so pion gathers this one on an ephemeral socket |

The only ways off the LAN that do not depend on the NAT type are therefore TURN (Home Assistant
Cloud or a user-run server) and a VPN.

The go2rtc setup used with c300x-controller solves this with `candidates: [stun:8555]`: it finds the
public IP through STUN and announces `<public IP>:8555`, with the port forwarded on the router.

## Proposal

The companion also announces `<public IP>:8555`, discovering the public IP through STUN. The new
candidate reaches the existing ICE mux on 8555. The phone contacts that address, and the companion
learns the phone's address from the incoming packets (peer-reflexive candidate), which should work
even when the phone is behind a symmetric carrier NAT.

- **Always on, no config key.** Without a forward the candidate simply fails and the other paths
  remain. The public IP is already exposed by the server-reflexive candidate, so nothing new leaks.
- **Additive.** Host, server-reflexive and relay candidates are unchanged, so LAN and TURN
  connections must behave as before.

## What the user has to do

Forward UDP 8555 on the home router to the intercom's LAN IP. The intercom firewall already accepts
that port.

## Limits

- The home line needs a real public IP: behind CGNAT at home the forward cannot work, and TURN or a
  VPN remain the only options.
- The external port must be 8555, unless a config key is added later. go2rtc lets the user choose it.
- UDP only. go2rtc also listens on TCP 8555; the companion would not.
- Off the LAN this path works only with the forward, so the docs must say so.

## Open points for the design

1. **Dynamic public IP.** `SetICEAddressRewriteRules` belongs to the `SettingEngine`, which is built
   once in `NewWebRTCService`. Either build the API per offer, or send the candidate at signaling
   level, reusing the IP from the server-reflexive candidate pion already gathers.
2. **Which STUN server.** Home Assistant's servers arrive per offer, while `defaultICEServers` is the
   fallback. Discovery must not delay the answer beyond `webrtcAnswerGatheringWait`.
3. **pion behaviour with the mux.** Confirm that a rewrite rule applies to the candidates of a plain
   `UDPMuxDefault` and keeps port 8555 (`gatherCandidatesLocalUDPMux` in pion/ice `gather.go`).
4. **Non-trickle offers.** The candidate must also be in the SDP answer for callers that do not trickle.
5. **Intercom firewall.** The server-reflexive port is random. On the C100X checked on 2026-11-05 the
   `INPUT` policy is `ACCEPT`, so that port is not blocked there. The installer's firewall hook expects
   a stock `iptables -P INPUT DROP`, so other models may differ: check a C300X.

## Verification

Needs a real intercom and a real mobile network, with these cases:

1. Mobile network, forward active: the log shows `webrtc candidate pair selected` and video plays.
2. Mobile network, no forward: same behaviour as today, with no added delay.
3. LAN: the host pair is still selected.
4. Home Assistant Cloud TURN active: relay still works when the direct path does not.

## Related documentation

The integration README ("Camera or card does not stream") tells users to check that UDP 8555 is
reachable from the browser's network, which suggests port forwarding even though it has no effect
today. Fix it and add a remote access section, listing TURN, VPN and, once shipped, the forward.

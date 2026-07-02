# C100X RTSP/WebRTC Media Design — AV Endpoint Commands + Call-State Gate

**Date:** 2026-07-02
**Status:** Approved for planning
**Scope:** Make the companion's RTSP/WebRTC stream work on the C100X. Phase 1 target: stream from idle. The same mechanism is designed to also cover the incoming-call case (486 avoidance), whose field validation is phase 2.

## 1. Problem

On the C100X, starting the stream from Home Assistant produces no video and no error: the RTSP `DESCRIBE/SETUP/PLAY` sequence succeeds but no RTP ever reaches the companion's ingest ports. The same device streams correctly (from idle) through the c300x-controller project, which is therefore the verified reference for how the C100X actually starts media.

A related, second symptom (observed with the c300x-controller): when someone rings from outside and a viewer tries to open the stream during the ring, the SIP INVITE gets `486 Busy Here` and no video starts.

## 2. Comparative analysis (companion vs c300x-controller)

### How the c300x-controller starts a stream (verified working on this C100X, idle case)

1. Sends a SIP INVITE (`webrtc@<domain>` → `c100x@...`, with a `DEVADDR` SDP attribute extracted from `mymodules` on C100X). The RTP ports in its SDP are **dummy** (65000/65002, intercom loopback): the SIP call only *arms* the intercom's AV subsystem.
2. The actual RTP flow is directed by a separate command channel: a **raw TCP** connection to `127.0.0.1:30007` (`bt_ipcamera` AV server), sending `*7*300#<ip-hash>#<port>#<quality>*##` frames (`bt-av-media.js`). No session handshake (unlike openserver:20000). Replies: `*#*1##` ACK (sometimes doubled `*#*1##*#*1##`), `*#*0##` NAK → retry up to 3 times with 1 s delay; 5 s idle timeout; the TCP connection is reused across commands.
3. Quality flag: video `#0` = high-res, `#1` = low-res, audio = `#2`. The controller forces **low-res on C100X** (`highResVideo=false`).
4. Video command first, audio command ~300 ms later.

### How the companion starts a stream today (dev/c100x)

1. SIP INVITE whose SDP advertises the **real** ingest ports (5000 audio / 5007 video) and relies on the device honouring the SDP. This works on the C300X; evidently not on the C100X.
2. A `CommandClient.StreamStart` exists that builds the **same** `*7*300#...` frames (`internal/protocol/openwebnet/frames.go`) but sends them to **openserver:20000** (with the openserver session handshake) instead of the AV server on 30007, and hardcodes high-res (`#0`) and IP `127.0.0.1`.
3. In `internal/services/media/composite_backend.go`, when SIP is enabled the command path is **never used** (neither as complement nor as fallback).

### Most probable root cause of the idle symptom

The INVITE (now targeting `c100x@...` after the maintainer's dev/c100x fixes) may well succeed, but nothing ever tells the C100X's AV server where to send RTP — the C100X does not honour the SDP ingest ports the way the C300X does. The RTSP client waits for packets that never come. (Alternative hypotheses and how the design covers them: §8.)

### Root cause of the 486 (incoming-call case)

`486 Busy Here` is the intercom's answer to an **outgoing** INVITE made while `bt_vct` is already engaged in the external call. The controller avoids this on the C300X by receiving the forked incoming INVITE (flexisip `route_int.conf` `alluser` provisioning) and answering it instead of dialing out. On the C100X the fork evidently never reaches it (the external call is handled by `bt_vct`/`bt_eliot` toward the cloud; additionally this unit has a stale `webrtc → 192.168.1.11` static route in `/etc/flexisip/users/route.conf` from an old setup), so a viewer connecting during a ring triggers a fresh outgoing INVITE → 486.

### Key insight

The `*7*300` command on 30007 does not create a call — it **adds an RTP destination to an already-running AV pipeline** (the controller calls it `addStream`). During an incoming call the pipeline is already armed by the call itself, so the right move is to *skip SIP entirely* and only send the AV commands. One mechanism therefore covers both scenarios.

### Flexisip provisioning is NOT needed

None of the c300x-controller README's "WebRTC linphone configuration" steps apply to this design:

| README step | Purpose in the controller | Needed by companion? |
|---|---|---|
| `webrtc` user in `users.db.txt` | Authenticate an external SIP client | No — companion runs on the intercom; `127.0.0.1` is in flexisip `trusted-hosts` by default |
| Static route in `route.conf`, `trusted-hosts`, transports on external IP | Route SIP to an external host (go2rtc/baresip) | No — everything stays on loopback |
| `webrtc` in `alluser` (`route_int.conf`) | Fork the doorbell INVITE to the controller | No — replaced by the call-state gate + 486 fallback |

The design modifies no flexisip files, so it survives DoorEntry app logout/login re-provisioning.

## 3. Chosen approach

**Approach A — AV endpoint adapter on port 30007, hooked after (or instead of) SIP, gated on call state.** Considered and rejected: a full port of the controller's event-driven re-arm choreography (over-engineered until proven necessary; kept as a phase-2 option) and an AV-commands-only backend without SIP (unsupported by the reference: the controller always couples a SIP call with the AV commands for the idle case).

All new behaviour is **model-gated**: the C300X path is unchanged.

## 4. Components

### 4.1 Generalized frame builders — `internal/protocol/openwebnet/frames.go`

- `BuildAVAddStreamVideo(ip string, port int, highRes bool) string` — `*7*300#<ip-hash>#<port>#0|1*##` (`0`=high, `1`=low).
- `BuildAVAddStreamAudio(ip string, port int) string` — `*7*300#<ip-hash>#<port>#2*##`.
- IP is encoded in hash form (`127.0.0.1` → `127#0#0#1`).
- Existing `BuildStreamStartVideo/Audio` become thin wrappers (or their call sites migrate) — no behaviour change for existing users.

### 4.2 AV media adapter — `internal/adapters/openwebnet/avmedia.go` (new)

Raw TCP client mirroring `bt-av-media.js` semantics:

- Connects to `MediaAVEndpointHost:MediaAVEndpointPort` (default `127.0.0.1:30007`). **No session handshake.**
- Writes a frame, reads the reply: `*#*1##` or `*#*1##*#*1##` → success; `*#*0##` → retry (max 3 attempts, 1 s apart); anything else → close connection, explicit error.
- 5 s idle timeout; connection reused across commands when still open.
- `StreamStart(ctx, audioPort, videoPort) error`: video command first, ~300 ms pause, then audio command.
- There is no "remove stream" command (the controller has none either); streams stop when the SIP call ends or the device tears the pipeline down.

### 4.3 Composite backend with call-state gate — `internal/services/media/composite_backend.go`

New optional dependencies: the AV adapter and a `CallStateProvider` (a function reading `CallState` from the state service: `Idle` / `Ringing` / `Active`).

`StreamStart` logic when the AV endpoint is enabled (C100X):

1. Read call state; log the value and the branch taken.
2. **Ringing/Active** → skip SIP (an INVITE would 486); send AV add-stream commands only. Record that no SIP call was opened.
3. **Idle** → SIP `StreamStart` (INVITE arms the AV pipeline), then AV add-stream commands toward the ingest ports.
   - **486 fallback (load-bearing):** if the INVITE fails with 486, treat it as authoritative evidence that a call is in progress — log "call in progress detected via 486" and proceed with AV commands only. This removes any dependency on ring events being visible on the openserver monitor (unverified on C100X: the controller receives them via multicast UDP 7667, a channel the companion does not listen on).
   - **Any other SIP failure** → still attempt the AV commands (best-effort), log both outcomes; return an error only if the AV commands *also* fail.
4. `StreamStop`: send SIP BYE only if this backend opened the SIP call; in the commands-only case there is nothing to stop.

When the AV endpoint is disabled (C300X / default off): current behaviour, byte-for-byte — existing `composite_backend_test.go` tests must keep passing (or be consciously adapted).

### 4.4 Config + wiring — `internal/config/config.go`, `internal/app/run.go`

New keys (JSON `config.json`, with normalization defaults):

| Key | Default | Notes |
|---|---|---|
| `MediaAVEndpointEnabled` | auto: `true` iff `DeviceModel == "C100X"` | explicit value in config always wins |
| `MediaAVEndpointHost` | `127.0.0.1` | |
| `MediaAVEndpointPort` | `30007` | |
| `MediaAVHighResVideo` | `false` | controller uses low-res on C100X |
| `DebugLogEnabled` | `false` | see §5 |
| `DebugLogPath` | `/tmp/companion-debug.log` | tmpfs by default (no flash wear); set a persistent path when a reboot-surviving log is needed |

`run.go`: when enabled, construct the AV adapter and pass it plus the call-state provider to `NewCompositeBackend`. The RTP destination IP used in AV frames is the loopback (companion runs on-device); host is configurable for completeness.

If SIP `From` identity ever matters on C100X (the controller uses `webrtc@<domain>`; the companion picks the first `users.db.txt` identity), it is already forcible via the existing `MediaSIPFrom` key — noted in the field checklist, no code change.

## 5. Debug logging to file

- `DebugLogEnabled=true` → logger output becomes `io.MultiWriter(stderr, file)`, hooked in `run.go` right after config load (`logger.SetOutput`). This matters because the init script discards stdout/stderr to `/dev/null`.
- Minimal rotation: when the file exceeds 5 MB, rename `.log` → `.log.1` and start fresh (bounds tmpfs usage).
- Always logged (debug or not): errors, StreamStart/Stop outcomes, SIP response codes.

### Diagnostic instrumentation (part of the work, not an extra)

Principle: **one failed field test must produce a log that pinpoints the broken stage without a second deploy.** Stages and the hypotheses their log lines discriminate:

| Stage | What is logged | Failure hypotheses it discriminates |
|---|---|---|
| 0. Startup | Build version, detected `DeviceModel`, resolved (sanitized) config: SIP target, DEVADDR, AV endpoint, ingest ports, debug flags | "wrong binary/config is running" |
| 1. Client request | RTSP session lifecycle (client IP, path, DESCRIBE/SETUP/PLAY, teardown reason); WebRTC offers + ICE state transitions | "HA never reaches us" vs "reaches us but gets no media" |
| 2. Gate | `CallState` value read at decision time + branch taken; *debug:* every raw monitor frame + mapped event, monitor connection state/reconnects | "gate decided wrong because the monitor was dead / state was stale" |
| 3. SIP leg | *debug:* full INVITE (target, From, DEVADDR, offer SDP); always: response code/reason, answer SDP (debug), registration outcome, BYE + response, flexisip TCP reachability | 404 (routing/user) vs 486 (busy) vs timeout (flexisip down — it is on-demand on C100X) vs 200-but-no-media |
| 4. AV leg | TCP connect outcome + latency to 30007; *debug:* every frame written and every raw reply, retries, connection reuse | "bt_ipcamera not listening" vs "rejects the command" vs "accepts but doesn't stream" |
| 5. RTP ingress | First packet per port (timestamp, src addr:port, payload type, SSRC); *debug:* packet/byte counters every 5 s; **warning if no RTP within 3 s of AV ACK**; first IDR/SPS seen on video | "device sends nothing" vs "wrong payload type" vs "undecodable video" vs "one port only" |
| 6. Egress | Forward counters to RTSP/WebRTC consumers, track write errors | "media enters but is not forwarded" |

Example signature of the current symptom ("PLAY ok, zero video, zero errors") under this instrumentation: stage 1 ok → gate Idle → INVITE 200 → stage 4 absent (no AV command in today's code) → "no RTP within 3 s" warning.

## 6. Data flow

**Idle (C100X):**
1. RTSP PLAY → `ReaderJoin` → composite `StreamStart`.
2. Gate: Idle → INVITE `c100x@<domain>` (arms AV pipeline); outcome logged with response code.
3. AV video command on 30007 (`*7*300#127#0#0#1#5007#1*##`, low-res), wait ACK, ~300 ms, AV audio command (`...#5000#2*##`).
4. RTP arrives on ingest ports 5000/5007 → existing RTSP/WebRTC/AudioBridge pipeline unchanged.
5. Last reader leaves → `StreamStop` → SIP BYE.

**Incoming call (C100X):**
1. Ring → (if visible) monitor event → `CallState=Ringing`.
2. PLAY → gate sees Ringing/Active → no INVITE → AV commands only. If the ring was *not* visible and an INVITE was sent, the 486 reply reroutes to AV-only (fallback §4.3).
3. `StreamStop`: no BYE (we did not open the call); the stream dies with the call.

**C300X:** untouched — pure SIP path as today.

## 7. Testing

Unit tests, runnable now without the device:

- **AV adapter** (`avmedia_test.go`, fake in-process TCP server mimicking `bt_ipcamera`): ACK; doubled ACK; NAK→retry→success; persistent NAK → error after 3 attempts; garbage reply → close + error; no reply → 5 s timeout; connection reuse across video-then-audio.
- **Frame builders:** low-res `#1` / high-res `#0` video, audio `#2`, hash-form IP.
- **Composite backend gate** (fake SIP + fake AV + fake call state): Idle → SIP-then-AV order; Ringing → AV only, SIP never called; Active → AV only; SIP 486 in Idle → AV attempted, no error, "no BYE on stop"; SIP non-486 failure in Idle → AV attempted, error only if AV also fails; stop-after-idle-start → BYE; stop-after-ringing-start → no BYE; AV disabled / C300X → existing behaviour (existing tests stay green).
- **Config:** defaults (auto-enable on C100X only, port 30007, low-res, debug off); JSON overrides win; log rotation at 5 MB.

## 8. Risks, alternative hypotheses, confidence

Idle-case alternative hypotheses and coverage:

| Hypothesis | Likelihood | Covered by |
|---|---|---|
| C100X ignores SDP ports, needs 30007 commands (primary) | High | The verified controller mechanism, replicated |
| INVITE fails silently (flexisip on-demand down, identity, DEVADDR 404) — invisible today because logs are discarded | Medium | Stage 0/3 logging; best-effort AV still starts video with SIP broken |
| Wrong DEVADDR extracted from `mymodules` | Low-medium | Stage 0+3 logs (value used + SIP response) |
| Unexpected payload type dropped at ingest | Low | Stage 5 log (PT/SSRC of first packet) |
| High-res unsupported on C100X | Medium (as co-cause) | Low-res default |

**Confidence:** idle case ~80–85% (the exact mechanism is field-verified on this same device by the controller; residual risk is edge differences such as the SIP From identity, forcible via `MediaSIPFrom`). Incoming-call case ~50–60%: one assumption remains that nobody has exercised on this device — that `bt_ipcamera` accepts an add-stream *while* the external call is active. The 486 fallback removed the other unknown (ring-event visibility). Worst-case outcome is no longer "still no video, no idea": the diagnostic matrix turns a failure into a targeted second iteration.

## 9. Field validation checklist (phase 2, when device access returns)

1. Deploy ARM build with `DebugLogEnabled=true` and a persistent `DebugLogPath`.
2. **Idle:** stream from HA → verify in log: gate=Idle, INVITE 200, AV ACK video+audio, first RTP on 5007/5000; video visible in HA.
3. **Incoming call:** external ring → PLAY from HA during the ring → verify: gate=Ringing (or 486-fallback line), no un-handled 486, AV ACKs, RTP ingress, video visible.
4. Try `MediaAVHighResVideo=true`; record whether the C100X supports it.
5. Config hygiene: remove the stale `webrtc → 192.168.1.11` static route from `/etc/flexisip/users/route.conf` (leftover from an old c300x-controller setup on another host).
6. If SIP behaves oddly, try forcing `MediaSIPFrom` to `webrtc@<domain>` (parity with the controller).
7. Regression: door unlock, events, snapshots keep working; C300X users unaffected (code inspection + tests, no C300X hardware here).

## 10. Out of scope

- Porting the controller's multicast (UDP 7667) listener or its event-driven re-arm choreography — phase-2 option if field validation shows one-shot AV commands are insufficient.
- Any flexisip file provisioning.
- HomeKit, RTSP auth/TLS, IPv6 — unrelated pre-existing gaps.

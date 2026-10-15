# Intercom Video While Ringing — Design

**Date:** 2026-10-15
**Status:** Approved for planning
**Repos:** `BTicinoGO` (companion), `BTicinoGO-Integration` (card, branch `feat/sip-incoming-call`)

---

## 1. Goal

Show the intercom video in the Home Assistant card while the doorbell is still ringing, so that the
video is already on screen when the user answers and the answer feels instant. Today the video
appears about 2 s after the answer.

## 2. What the logs and the code show

A real doorbell call on 2026-10-15:

| Time | Event |
|---|---|
| 14:18:52 | INVITE arrives, companion answers 180, the card rings |
| 14:18:54 | The intercom starts its own AV on its own (`external stream detected`, owner `external`) |
| 14:19:09 | Answer: 200 OK, ACK, RTP keepalive starts. Intercom audio appears only now |
| 14:19:10 | A second INVITE arrives and is closed with no companion log line (see section 9) |
| 14:19:11 | The card sends its offer, the lease adopts the external stream (`adopted_external=true`, no INVITE) |

The video exists 15 s before the answer. A preview during ringing is refused locally, without any
INVITE, in two places:

1. `StreamCoordinator.reserve` (`internal/media/stream_coordinator.go`): owner `External` and no
   answered call → `ErrExternalStream`.
2. `Manager.StartStream` (`internal/signaling/manager.go`): `m.incoming != nil` → `ErrIncomingDialog`.

Removing both is not enough, for two further reasons found in the code:

- `AVClient.Start` (`internal/openwebnet/av.go`) requests video and then audio, and fails after
  three attempts if audio does not flow. Intercom audio is not there before the answer.
- **Closing such a preview would reject the doorbell call.** A consumer release stops the lease
  with `causeConsumerReleased` (`endsCall: true`), which calls `EndCallOnClose`; `SourceSession.Close`
  then calls `Manager.Hangup`. With no active dialog, `Hangup` takes the ringing dialog and answers
  it **`603 Decline`**. A 6xx ends every fork, so the indoor unit would stop ringing too.

Contrary to an earlier assumption, the companion never sends the bare AV stop `*7*0*##`: the frame
is only recognised on input (`openwebnet.IsStop`). Its stream teardown is closing its own sockets
and, when it owns the dialog, a BYE.

## 3. Scope

- During ringing, only the **card** (any WebRTC consumer) may adopt the intercom stream. RTSP and
  HomeKit stay refused until the call is answered, as today.
- The **indoor unit** must keep ringing and showing its video undisturbed. This is a constraint,
  not a consumer.
- The card shows the preview only while it is open. It does not need to be open before the ring:
  opening it during the ring starts the preview then. With no card open nothing is requested.
- The companion does **not** start the video on its own at every ring.

## 4. Design

### 4.1 Lease phase

A lease gains a phase:

- **`Ringing`** — video only, backchannel (microphone) blocked, no path may end or reject the call.
- **`Answered`** — today's behaviour.

Every lease that is not a ringing preview starts in `Answered`.

### 4.2 Who may preview while ringing

`WebRTCService` acquires with an option `AllowRingingPreview`; RTSP and HomeKit do not pass it. The
card sends no new parameter: being a WebRTC consumer is what qualifies it. The Home Assistant
camera view also uses WebRTC and gets the preview too, which is intended.

The coordinator gets a second probe, *call ringing*, next to the existing answered-call probe, and
taken the same way (outside `c.mu`). In `reserve`:

| Ringing | Answered | Option | Result |
|---|---|---|---|
| yes | — | yes | lease in `Ringing`, adopts whatever the owner is (`External` or `Idle`) |
| yes | — | no | refused (`ErrIncomingCall`), whatever the owner is |
| no | yes | any | lease in `Answered`, adopts (today) |
| no | no | any | today's rules |

The ringing row must not depend on the owner being `External`: a card that closes and reopens its
preview during the ring finds the owner `Idle`, and RTSP must still be refused then.

### 4.3 Signaling: a media session never rejects a ringing call

`Manager.Hangup` does two different things today: a BYE on an active dialog, and a 603 on a ringing
one. They are separated:

- **`HangupCall`** (new) — sends a BYE on the active dialog, and is a no-op when the call is only
  ringing or when nothing is up. It is the only hangup on the `SourceSIP` interface; both BYE sites
  in `SourceSession` call it.
- **`Hangup`** (unchanged) — still used by `POST /api/v3/call/hangup`, where ending a ringing call
  with 603 is a user action.

`Manager.StartStream` keeps refusing a ringing call (`ErrIncomingDialog`). A session prepared as a
ringing preview calls **`AdoptRingingCall`** instead: it never dials, reports whether the call is
already answered, and fails with `ErrNoInboundCall` once the ring is over. Keeping the two apart
means an RTSP or HomeKit source that races past the coordinator's gate still cannot attach to the
ring.

The invariant is held by the manager, not by the session's state: whatever phase a session believes
it is in, the media path cannot produce a 603. This closes the class of defect from
`2026-08-06-media-teardown-call-safety-design.md` one layer lower.

### 4.4 Source session and AV

- `SourceAV.Start` takes the tracks to start. In `Ringing` the session starts video only; the audio
  receiver socket is still bound, so its port is ready for later.
- `SourceAV.StartAudio` (new) sends the audio `*7*300#...#2*##` request with the existing retry and
  flow-wait logic.
- `SourceSession.Answered(ctx)` (new) calls `StartAudio` on the already-bound audio port. Video,
  sockets, lease and the WebRTC session are untouched.

### 4.5 Coordinator

- **`PromoteRinging()`** — moves the `Ringing` lease to `Answered`: unblocks the backchannel and calls
  `Answered` on the source. If the lease is still starting, it records a pending promotion and
  `finishStart` runs it as soon as the source is up.
- **`StopRinging()`** — stops a `Ringing` lease with a new cause `causeRingingEnded`
  (`endsCall: false`). If the lease is starting, the start is cancelled, as `stop` already does.
  No-op on an `Answered` lease or with no lease.
- **`WriteBackchannelRTP`** drops packets while the lease is `Ringing`.
- **Stall watchdog**: must only consider requested tracks, or the missing audio in `Ringing` would
  stop the lease as `causeTracksStalled`. To be confirmed in the plan.

### 4.6 Event wiring (`internal/app/run.go`)

| Event | Action |
|---|---|
| `CallAnswered` | `PromoteRinging()` |
| `IncomingCallEnded` (cancelled, timeout, elsewhere) | `StopRinging()` |
| `CallDeclined`, `CallHungUp` | `StopRinging()` |

The companion closes the preview on its own when the ring ends; it does not rely on the card.

### 4.7 Card (`www/bticino-go-intercom-card.js`)

- When the entity is ringing and the card is visible, start the WebRTC session.
- The microphone transceiver is in the offer from the start, with no track (`replaceTrack(null)`).
- On answer: send `card_call_answer` and `replaceTrack(mic)`. No renegotiation, same session.
- When the ring ends without an answer from the card, close the session.

The microphone is blocked twice before the answer: the card sends no audio, and the coordinator
drops backchannel packets to `127.0.0.1:4000` while `Ringing`.

## 5. Flows

**Preview start**

1. INVITE → 180 → `IncomingCallStarted`. The card shows the ring.
2. If the card is visible it sends its offer. The coordinator sees a ringing call and the option:
   lease in `Ringing`.
3. The session binds both sockets, `StartStream` adopts without INVITE (`owns=false`), AV requests
   video only. Video reaches the card.

**Outcomes**

| Outcome | Event | Effect | Call ended by the companion |
|---|---|---|---|
| Answered from the card | `CallAnswered` after the 200 OK | `PromoteRinging`: backchannel unblocked, `StartAudio`. Card `replaceTrack(mic)` | From now on, on close (today's behaviour) |
| Cancelled by the outdoor station | `IncomingCallEnded{cancelled}` | `StopRinging`, sockets closed, card closes | No |
| Timed out | `IncomingCallEnded{timeout}` + 480 | as cancelled | No |
| Answered elsewhere | `IncomingCallEnded{elsewhere}` | as cancelled | No |
| Rejected from the card | `CallHungUp` (603 sent by `Hangup` via the API) | `StopRinging` | The 603 is the user's |
| Card closed during the ring | consumer release | Only the preview sockets close. No SIP, no AV. The call goes on unchanged | No |

The companion's 60 s expiry answers 480 on its own fork only; the indoor unit's ring ends on the
intercom's or Flexisip's own timer, which the card never affects.

**Races**

- *Answer vs end of ring*: the manager serialises them and publishes only one of the two events;
  events reach the coordinator in order from the single drain goroutine.
- *Answer while the preview is still starting*: pending promotion, run by `finishStart`. If the
  answer lands before `reserve`, the lease is born `Answered` and starts in full, as today.
- *End of ring while the preview is starting*: `StopRinging` cancels the start.

**Promotion failure**

If `StartAudio` fails after its three attempts, the lease stops with `causeSourceFailure`
(`endsCall: false`): the call stays up. The card sees its session close and opens a normal one,
which adopts the answered call's stream — today's behaviour, video about 2 s later, instead of a
lost call. Logged at `Warn` with the reason.

**Invariant:** before `CallAnswered`, no path of a media session sends a BYE or a 603.

## 6. Testing

| Package | What the tests pin | Where they run |
|---|---|---|
| `signaling` | `StartStream` with a ringing call returns `(false, nil)` and never reaches the dialer. `HangupCall` while ringing sends nothing; on an answered call sends the BYE. `Hangup` still answers 603 to a ringing call | Windows |
| `media` (coordinator) | The `reserve` table of 4.2. Promotion of a starting lease. `StopRinging` without `EndCallOnClose`. Backchannel dropped in `Ringing`. Stall watchdog on requested tracks only | Linux only |
| `media` (session) | `Ringing` starts video only. `Answered` calls `StartAudio`. With a ringing call, `Close` under every stop cause — `EndCallOnClose` included — leaves the fake SIP with no BYE and no 603 recorded | Linux only |
| `openwebnet` | `StartAudio` frame; `Start` with the track selection | Windows |

On Windows: `GOOS=linux GOARCH=arm GOARM=7 go build ./...` and `go vet`. The `internal/media` tests
are written but not executed locally.

## 7. Implementation steps, each verified with one ring

1. **Manager, coordinator, video-only session, card starts the preview on ring.**
   - Card open at the ring: video within about 1 s, **indoor unit undisturbed**. This is the
     go/no-go check: if it fails, stop here.
   - Ring left to time out: the card closes, no 603 in a SIP capture.
   - Card closed during the ring: the indoor unit keeps ringing.
2. **Promotion on answer.** Answer from the card: video uninterrupted, two-way audio within about
   1 s.
3. **Card finishing, answered elsewhere.** Answer on the indoor unit: the card closes, the indoor
   conversation is normal.

## 8. Alternatives rejected

- **Two leases in sequence** (video-only preview, then a full lease at the answer, same WebRTC
  session on the card). Simpler state, but the RTP ports change, a gap of a few hundred ms is
  possible, and there is a race window between release and the new acquire. Kept as the fallback
  if adding audio to an open session turns out not to work on the intercom.
- **Full session started early by the card.** Needs intercom audio before the answer, which the
  logs say is not there.
- **Companion starts the video at every ring.** Saves only the AV request, a few hundred ms, once
  the card opens; the WebRTC negotiation is still needed, and the intercom gets requests when
  nobody is watching.

## 9. Out of scope, recorded

- **The second INVITE at 14:19:10.** With a call active, `Manager.OnInvite` answers `486 Busy Here`
  without logging. If it is a re-INVITE inside the answered dialog (same Call-ID, To with tag), a
  486 is the wrong answer. Needs a capture to tell; a separate fix.
- **The `486 Busy Here` at 16:51:54**: a preview started about 11 s after a cancelled call, with the
  intercom still busy. Not affected by this design.

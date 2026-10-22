# Shared Intercom Stream and Doorbell Snapshot — Design

**Date:** 2026-10-22
**Status:** Approved for planning
**Repos:** `BTicinoGO` (companion), `BTicinoGO-Integration` (integration and card, branch `v3`)

---

## 1. Problems

Three defects reported after the ringing preview (2026-10-15) shipped:

1. **The notification picture is stale.** A Home Assistant notification sent on a doorbell ring
   shows an old picture, often the previous call.
2. **Two devices cannot both preview.** With the card open on two devices, when someone rings the
   second device does not get the stream.
3. **A second "Ringing" after the call.** After answering and hanging up, `binary_sensor.*_ringing`
   turns on again and the ring notification is sent a second time.

## 2. What the code shows

### 2.1 Stale snapshot

`SnapshotManager` is passive (`internal/media/snapshot.go`). A capture is armed only in
`newBridgeSource` (`internal/app/run.go`), that is, only when some consumer opens a media source.
Nothing captures on a ring. The camera entity's `async_camera_image` returns
`/snapshot/latest.jpg`, which is the last frame captured by whoever last opened the stream. Even
with a card open, the new JPEG lands 2–3 s after the ring, so an automation that fires on the ring
reads the old file.

### 2.2 Second device

The coordinator holds one lease with one consumer. `WebRTCService.Offer` closes every other session
of the same entrypoint as "superseded by successor session" and hands the lease over
(`Handover`). That rule exists for Home Assistant opening a successor player before the previous
one closed; it treats a second device the same way. During a ring:

- both cards offer at once; the second waits on `offerMu` for the first's startup, then supersedes
  it. The first card does not retry (one preview attempt per call);
- a superseded ringing preview sends neither BYE nor AV stop, so the intercom keeps sending video
  to the first session's closed port. The second session asks for video on a new ephemeral port
  (`*7*300#…`) and most likely sees no flow within the timeout. The card swallows preview errors,
  so the user sees nothing.

### 2.3 Second "Ringing"

`deriveCallState` (`internal/core/state.go`) returns `ringing` when `PhysicalRing` or
`IncomingCall` is set and there is no `ActiveCall`. `PhysicalRing` is set by the OpenWebNet ring
frame and cleared only by a stream-stop or free-AV-resources frame (`internal/openwebnet/mapper.go`).
Answering does not clear it. On hangup `CallHungUp` removes `ActiveCall` first; until the bus stop
frame arrives the state is `ringing` again, the state is pushed, and Home Assistant fires.

## 3. Scope

- **Multiple WebRTC viewers share one intercom stream, always**: during the ring, after the answer
  and in an idle live view. A new offer joins; it never closes another session.
- RTSP and HomeKit are unchanged: their leases stay exclusive, and they stay refused while the
  call rings.
- **The companion opens the stream itself on every SIP ring** (the "ring holder") to take the
  doorbell picture. This reverses the 2026-10-15 rule "the companion does not start the video on
  its own at every ring"; the ringing preview already does exactly this whenever a card is open, so
  the indoor unit is known to tolerate it. The reason: the companion receives video only when it
  requests it — the AV the intercom starts on its own while ringing goes to the indoor unit and is
  seen only as an "external stream" — so with no card open there is no frame to take a picture
  from. The holder keeps the stream until the answer or the end of the ring rather than closing it
  after the picture: closing a ringing preview sends no BYE and no AV stop, and a card opening
  afterwards would have to ask the intercom for video on a new port mid-ring, the case suspected
  in 2.2 of not working. Joining the running stream avoids that request altogether.
- When one device answers, **the other cards close their view** and show "In a call on another
  device". Only the answering device gets audio and microphone.
- Home Assistant gets an **image entity "Last doorbell ring"** per entrypoint, updated only by a
  doorbell picture. Notification automations trigger on it.
- The physical ring is cleared when the SIP call is answered or ends (fixes 2.3).

Out of scope: RTSP/HomeKit joining a shared stream; any change to how the call is answered or
rejected on the SIP side.

## 4. Design — companion

### 4.1 Shared lease (`internal/media`)

The coordinator still owns one intercom stream at a time, but a stream may have several consumers.

- `StreamLease` becomes the consumer's handle: `{stream id, consumer id}`. RTSP and HomeKit code is
  unchanged.
- An acquire is **shareable** or **exclusive**. `AcquireRingingPreview` (WebRTC) and the ring
  holder's acquire are shareable; `Acquire` and `AcquireWithStartup` (RTSP, HomeKit) are exclusive.
  A stream remembers whether it was born shareable.
- `reserve`:
  - no stream → a new stream, as today (same ringing/answered/external rules);
  - stream exists, born shareable, same entrypoint, not stopping, and the acquire is shareable →
    **join**: a new consumer handle on the existing stream;
  - anything else → `ErrStreamBusy`, as today.
- **Joining a starting stream** waits for its startup to finish, bounded by the joiner's
  `startupCtx`. If startup fails, the joiner gets the same error. A `started` channel per stream
  carries this.
- **Fan-out.** The wrapped `SourceEvents` copy each video and audio RTP packet to every consumer.
  The consumer list is copied under `c.mu` and invoked outside it, in the order consumers joined.
- **Stream-level events** — remote BYE, source failure, stall, OpenWebNet stop, end of ring,
  failed promotion — stop the stream and notify **every** consumer with the same signal each gets
  today (`RemoteBYE`, `Failed(ErrRingingPreviewLost)`, `Failed(ErrRingingEnded)`, …).
- **A consumer leaving:**
  - `Release(handle)` — the ordinary close. If other consumers remain, only this one is detached.
    If it was the last, the stream stops with `causeConsumerReleased` (`endsCall: true`), as today:
    closing the last card hangs up.
  - `Leave(handle)` — never ends the call. If it was the last consumer, the stream stops with a new
    `causeConsumerLeft` (`endsCall: false`). Used by the ring holder and by a card that closes
    because the call was answered elsewhere.
  - Both are no-ops on a handle that already left or whose stream is gone.
- `Handover` and `causeConsumerSuperseded` are removed; nothing uses them any more.
- **Backchannel.** `WriteBackchannelRTP` accepts any current consumer handle of the stream, with
  the existing starting/stopping/ringing checks. Only the answering card has a microphone.
- `PromoteRinging`, `StopRinging` and the stall watchdog act on the stream, not on a consumer, and
  keep their current semantics.

### 4.2 Fan-out and keyframe cache (`internal/media/fanout.go`, new)

A self-contained type owned by each stream:

- holds the consumer set (`add`, `remove`, `len`, `forEach`);
- keeps a **keyframe cache**: the video packets from the most recent H.264 IDR (NAL 5) or SPS
  (NAL 7) onward, detected in a single-NAL payload, a STAP-A aggregate or the start fragment of an
  FU-A. A new IDR/SPS start resets the cache. Cap: 512 KB of payload; past it the cache is dropped
  and stays empty until the next IDR/SPS;
- on `add`, the new consumer is first sent the cached packets, then live ones. Replay and the
  insertion into the live set happen under the fan-out's own lock, so no live packet can slip in
  between: the sequence numbers are contiguous. The video appears immediately instead of after the
  next intercom keyframe, whose interval is unknown.

Audio is not cached.

### 4.3 WebRTC service

- `prepareOffer` no longer collects sessions to supersede; `Offer` no longer closes them. Each
  session gets its own handle from `AcquireRingingPreview`, which joins when a shareable stream
  exists.
- `closeSessionWith` keeps its shape with `Release` or the new `Leave` as the drop function.
- `Close(sessionID)` keeps meaning `Release`. A new `Leave(sessionID)` uses `Leave`.
- `offerMu` stays: offers are still serialised, but the second one now joins instead of tearing
  the first down.

### 4.4 Ring holder (`internal/app`)

A small component driven by the call events that already reach the code calling
`PromoteRinging`/`StopRinging`:

- `IncomingCallStarted{EntrypointID}` → `AcquireRingingPreview` for that entrypoint with empty
  consumer events, on its own goroutine with the WebRTC startup timeout. It joins if a card was
  first. A failure is logged as a warning and nothing else happens; no retry.
- `CallAnswered` → after `PromoteRinging` (unchanged), the holder calls `Leave`. With no card
  attached the stream stops without BYE: the call stays answered and RTSP/HomeKit may adopt it, as
  before this change.
- `IncomingCallEnded`, `CallDeclined`, `CallHungUp` → `StopRinging` (unchanged) stops the stream;
  the holder drops its handle.
- An `IncomingCallStarted` while the holder still has a handle drops the old one first (`Leave`).

### 4.5 Doorbell snapshot

- `SnapshotManager.Arm(entrypointID, ring bool)`. `bridgeSource` passes `ring = true` when
  `PrepareRingingPreview` was called before `Start`, so the attempt is armed in
  `bridgeSource.Start` rather than at construction.
- `publish` always writes `snapshots/<id>.jpg`. A ring attempt also writes
  `snapshots/<id>-ring.jpg` (same atomic temp-file + rename).
- `Latest(id)` is unchanged. New `LatestRing(id)`.
- Capture timeout stays 8 s. Expected latency from the ring: about 2–3 s (INVITE, video start,
  first IDR, decode).

### 4.6 API and state

- New route `GET /api/v3/entrypoints/{id}/snapshot/ring.jpg`, same auth and errors as
  `latest.jpg`.
- `EntrypointDTO` gains `snapshot_updated_at` and `ring_snapshot_updated_at` (RFC 3339, omitted
  when the file does not exist), read from the files' modification time so they survive a restart.
- `snapshots.SetOnCaptured(server.BroadcastState)` already pushes the new state after every
  capture.
- The WebRTC websocket `close` message accepts `"leave": true`, which calls `WebRTCService.Leave`.
  Closing the websocket itself still releases its sessions.

### 4.7 Physical ring cleared by the SIP call (`internal/core`)

In the reducer, `CallAnswered`, `CallDeclined`, `CallHungUp` and `IncomingCallEnded` also clear
`PhysicalRing` when it belongs to the same entrypoint as the call. A later bus stop frame then
finds no ring and its `RingCleared` is a no-op, as today. After a hangup the state goes straight
from `active` to `idle`.

## 5. Design — Home Assistant

### 5.1 Integration

- `Entrypoint` model reads `snapshot_updated_at` and `ring_snapshot_updated_at`.
- `CompanionApiClient.async_entrypoint_snapshot_ring(entrypoint_id)` fetches `ring.jpg`.
- New `image` platform: one `ImageEntity` per entrypoint, name **"Last doorbell ring"**
  (translated "Ultima citofonata"). `image_last_updated` comes from `ring_snapshot_updated_at`, so
  the entity state changes only on a new doorbell picture. `async_image` fetches `ring.jpg` and
  caches the bytes until the timestamp changes. State is `unknown` before the first ring picture.
- `async_close_card_webrtc_session(session_id, leave=False)`; with `leave` the companion `close`
  message carries `"leave": true`. Home Assistant's own `close_webrtc_session` keeps releasing.
- README: an example automation — trigger on the state of the "Last doorbell ring" image entity,
  notify with `image: /api/image_proxy/<that entity_id>`. The entity id depends on the
  installation's language, so the README tells the user to pick it from the entity list.

### 5.2 Card

- Ringing preview: unchanged (one attempt per call); the offer now joins the shared stream and,
  with the ring holder up, gets the cached keyframe at once.
- **Answered elsewhere**: when the call state becomes `active` and this card is not answering
  (`!_answering`), the card closes its session with `leave` and shows "In a call on another device".
  It opens nothing until the call ends. This replaces today's listen-only viewer.
- Answering from this card: unchanged. With a preview it adds the microphone to its session;
  without one it offers, which now joins.
- Hangup and closing the answering card: unchanged; the last `Release` sends the BYE.

## 6. Error handling

| Case | Behaviour |
|---|---|
| Ring holder cannot open the stream | Warning; ring and cards proceed as today. No new ring picture, so the image entity does not change and no notification fires. Documented in the README. |
| No JPEG within 8 s | As above; warning logged. |
| Card joins a starting stream whose startup fails | Every joined consumer gets the error; the preview stays silent as today. |
| Stall, OpenWebNet stop or remote BYE with several cards | Stream stops; every consumer is notified. During the ring every card gets `ErrRingingPreviewLost`. |
| Promotion fails | As today: the call stays up, the stream stops, the answering card re-offers. |
| Keyframe cache exceeds 512 KB | Dropped until the next IDR/SPS; a joiner at that moment waits for the next keyframe. |
| `Leave`/`Release` on a stale handle | No-op. |

## 7. Testing

- `internal/media` — unit tests for `fanout.go` (fan-out order; IDR detection in single NAL,
  STAP-A and FU-A; cap; contiguous replay), the coordinator (join, join while starting, startup
  failure seen by joiners, `Release`/`Leave` of the last and of a middle consumer, ring holder with
  promotion, RTSP still exclusive, stream-level events reach every consumer), WebRTC (no
  supersede; `Leave`), and `SnapshotManager` (ring file written only by ring attempts). These do
  not build on the Windows host: verify with `GOOS=linux go build ./...`; execution is left to CI
  or the device.
- `internal/core` — reducer tests: answer, decline, hangup and incoming-end clear the physical
  ring; after hangup the state is `idle`, never `ringing`. Run natively.
- `internal/app`, `internal/api` — ring holder driven by events; `ring.jpg` route;
  `*_updated_at` fields; websocket `close` with `leave`.
- Integration — pytest for the image entity (state from timestamp, cache, initial `unknown`), the
  model fields and the `leave` flag.
- Manual, on the intercom: two devices both show the ringing preview; answering on one closes the
  other with the message; the notification picture is from this ring, about 3 s after it; no
  "Ringing" after hangup.

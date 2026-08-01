# Answering Incoming SIP Calls from the Home Assistant Card — Design

**Date:** 2026-08-01
**Status:** Approved for planning
**Repos:** `BTicinoGO` (branch `main` ≡ `upstream/v3`), `BTicinoGO-Integration` (branch `v3`)

---

## 1. Problem

When the outdoor station rings, the Home Assistant card cannot take the call. The user sees an
"Incoming call" overlay and nothing else actionable.

The cause is not a single bug. The answer path is absent at all three layers of the v3 stack.

### 1.1 Card

`custom_components/bticino_companion/www/bticino-go-intercom-card.js` hides the green button
precisely while ringing:

```js
<button class="call ${active || ringing ? "hidden" : ""}" title="Start live view">
<button class="end  ${active ? "" : "hidden"}"           title="End live view">
```

During `ringing` (not yet `active`) neither call button is rendered. Only the ring overlay and the
unlock button remain. The two buttons that exist are "Start / End live view", not answer / decline.

`docs/v3-intercom-card-plan.md` on the same branch specifies *"Ringing → green = Answer the SIP
call, red = Decline the SIP call"*. That was never implemented.

### 1.2 Integration

The v3 integration has no answer service, no API client method and no button. It exposes only the
`ringing` binary sensor, the `call_state` sensor and the WebRTC camera. There is no control loop.

### 1.3 Companion

- `internal/signaling/dialer.go:133` registers **only** `server.OnBye`. There is no `OnInvite`,
  `OnCancel` or `OnAck`, so an inbound INVITE is never processed.
- `internal/signaling/manager.go` already contains correct `OnInvite` / `Answer` / `Decline` /
  `Hangup` logic, but it is orphaned: `OnInvite` has no production caller, and the instance created
  at `internal/app/run.go:554` is constructed with `events = nil`. `media.SourceSession` uses only
  its `StartStream` / `Hangup` methods.
- Ringing is detected solely through OpenWebNet. `internal/openwebnet/mapper.go:48` matches
  `*8*1#1#4#…` and **synthesises a fake dialog** `openwebnet-<timestamp>`, from which
  `IncomingCallStarted` is emitted. The card therefore shows "ringing", but that dialog ID
  corresponds to no SIP dialog, so there is nothing to answer.
- `internal/api/api.go:76-102` exposes no `call/*` route.
- The SIP identity is hardcoded to `companion@127.0.0.1:5070` over TCP with no credentials:
  `internal/app/run.go:273` passes only `Target` and `Domain`, and `internal/config/config.go` has
  no SIP fields at all.

### 1.4 What already works

The media half is complete and must not be rebuilt. Browser microphone → WebRTC →
`coordinator.WriteBackchannelRTP` → audio bridge → speex → intercom is wired end to end
(`internal/media/webrtc.go:300-306`, `internal/media/stream_coordinator.go:352-371`). Two-way audio
already functions in preview. **Only call control is missing.**

---

## 2. Delivery strategy (decided)

For the companion to answer, Flexisip must fork the INVITE to it. Three delivery strategies were
considered:

| Strategy | Summary | Outcome |
| --- | --- | --- |
| **A. Dedicated Flexisip user** | Provision `companion@<domain>` in `users.db.txt`, `route.conf` and the `<sip:alluser@…>` group in `route_int.conf` | **Chosen** |
| B. Additional contact on an existing AOR | Register a second contact for `c300x@<domain>`, already a member of `alluser`, without editing any BTicino file | Rejected for now — risk that `max-contacts-by-aor=1` evicts the monitor's own registration |
| C. No inbound SIP | Auto-open preview on ring; no real answer | Rejected — the physical monitor keeps ringing, and it depends on the unverified assumption that an outbound INVITE succeeds during a ring |

Strategy A is the one proven to work on this hardware by the `c300x-controller` reference project.

Because the companion runs on the device itself at `127.0.0.1`, and `flexisip.conf` sets
`trusted-hosts=127.0.0.1`, digest authentication is bypassed. No password is required — unlike the
reference's go2rtc setup, which runs on a remote host.

**Decision:** provisioning is performed **only by the installer**, with no runtime detection.

---

## 3. Scope

### In scope

- Inbound SIP INVITE handling in the companion, with `180 Ringing` and a `200 OK` answer.
- `POST /api/v3/call/answer` and `POST /api/v3/call/hangup`.
- Media activation after answering, without an outbound INVITE.
- Home Assistant services, WebSocket commands and card buttons for Answer and Hang up.
- An explicit "answered elsewhere" state.
- Unlock available during an active call (already exists; must remain reachable).
- Idempotent Flexisip provisioning in `scripts/install.sh`.

### Out of scope

- **Decline.** Not requested. `Manager.Decline` stays in the codebase for the concurrent-call
  `486` path but is not exposed over HTTP.
- **Video preview before answering.** Ringing shows no stream; video and audio start on answer.
  This deliberately avoids the call-waiting problem — whether an outbound INVITE to
  `c300x@<domain>` returns `486 Busy` while the intercom is ringing is never exercised.
- **Runtime re-provisioning of Flexisip files** and any detection of their loss.

---

## 4. Architecture

The guiding idea: do not add a second call system alongside the existing one. Reconnect what the v3
code already presumes but never wires — a **single shared `signaling.Manager`** that owns both the
inbound and the outbound dialog and is the sole source of truth for call state.

### 4.1 End-to-end flow

1. **Startup** — the companion reads the new SIP configuration section and passes real
   `From` / `AuthUser` / `Listen` / `Transport` values to `NewStreamDialer` (today ignored). It
   registers as `companion@<domain>` and listens for INVITEs.

2. **Ring** — the outdoor station calls `alluser@<domain>`; Flexisip forks to the monitor, the
   DoorEntry app clients and the companion. Two signals arrive in parallel, each feeding its own
   state field:
   - OpenWebNet frame `*8*1#1#4#…` → `RingStarted` → `PhysicalRing`
   - SIP INVITE → `180 Ringing` → `IncomingCallStarted` carrying the **real Call-ID** →
     `IncomingCall`

   `deriveCallState` (`internal/core/state.go:232`) already maps both to `ringing`, so the card
   shows the ring even if one source is late. The synthetic `openwebnet-<ts>` dialog is removed.

3. **Answer** — the card calls `POST /api/v3/call/answer` → `Manager.Answer` sends `200 OK` with an
   SDP containing `a=DEVADDR:` (using the resolved entrypoint's devaddr) → `CallAnswered` → state
   `active`. SIP forking should make the proxy CANCEL the other branches, including the physical
   monitor, which then stops ringing. That is the desired behaviour and comes free from the
   protocol — but the monitor's ring is also driven internally over OpenWebNet, so whether it
   actually falls silent is an expectation to confirm during on-device verification (§9), not a
   guarantee of this design.

4. **Media** — the card then opens a WebRTC session through the existing `card_webrtc_offer` path
   with the microphone enabled, exactly as it does for preview today. That path already runs
   `WebRTCService.Offer` → `startOfferSource` → `coordinator.AcquireWithStartup` → `newBridgeSource`
   → `SourceSession.Start` → `Manager.StartStream` + `AVClient.Start`. Because `StartStream` is a
   no-op while a call is answered, the source starts **without an outbound INVITE**, sends the usual
   `*7*300#…` frames to port 30007, and clear RTP flows on 5007 / 5000 → distributor → WebRTC.

   **`/api/v3/call/answer` therefore performs only the SIP answer.** It does not acquire a lease and
   does not start a source: the media path is entirely unchanged and needs no new code. This is the
   design's largest simplification.

5. **Teardown** — three converging paths: Hang up → BYE; remote BYE → `OnBye`; CANCEL because the
   call was answered on the monitor → `IncomingCallEnded{Reason: elsewhere}`, surfaced by the card
   before returning to idle.

### 4.2 The leverage point

`Manager.StartStream` currently rejects with `ErrActiveDialog` when a dialog is already active
(`internal/signaling/manager.go:148`). When the active dialog is an **answered incoming call**, it
must instead return success without sending anything. With that change `media.SourceSession` needs
no modification at all: it proceeds to `AVClient.Start` and the rest of the chain unchanged.

The only structural cost is wiring. Today `internal/app/run.go:554` creates a **new Manager per
source** with `events = nil`. A single instance must be created in `newRuntime`, connected to the
dialer's `OnInvite` and to the event sink, and passed into `newSource`.

---

## 5. Companion components

### 5.1 Configuration — `internal/config`

New `companion.sip` section. Defaults reproduce today's behaviour exactly, so an un-migrated
installation does not change:

```yaml
companion:
  sip:
    from:      "companion@127.0.0.1:5070"
    auth_user: ""          # empty → the user part of `from`
    auth_pass: ""          # empty → no digest auth (trusted-hosts=127.0.0.1)
    listen:    "127.0.0.1:5070"
    transport: "tcp"
    domain:    ""          # empty → DiscoverFlexisipDomain()
    inbound:   false       # master switch for call answering
```

`inbound: false` is deliberate. Answering only works if the Flexisip user has been provisioned. An
explicit flag prevents an un-provisioned installation from registering handlers that will never
fire and showing a button in Home Assistant that does nothing — which is today's bug in a new form.

`run.go` stops building `StreamDialerConfig` from two fields and passes the whole section.

### 5.2 Dialer — `internal/signaling/dialer.go`

The dialer already owns a `sipgo.Server` and a `DialogClientCache`. The server side is added:

- `sipgo.NewDialogServerCache(client, contact)` alongside the existing client cache;
- `server.OnInvite` → `ReadInvite` → delegate to the `Manager`, which decides `180` or `486`;
- `server.OnCancel` → `200 OK` plus a `Manager` notification (the "answered elsewhere" case);
- `server.OnAck` → `dialogs.ReadAck`, required for the inbound dialog to be considered established;
- the existing `OnBye` extended: try `out.ReadBye` first (outbound dialog, current behaviour), then
  `dialogs.ReadBye` (inbound dialog).

All of the above are registered only when `inbound: true`.

The existing registration loop (`internal/signaling/dialer.go:192`) is unchanged except that
`From` / `AuthUser` now come from configuration instead of hardcoded defaults.

### 5.3 Manager — `internal/signaling/manager.go`

The logic mostly exists. Changes:

- **`SetEvents(EventSink)`** — the sink is currently constructor-only and is not available at
  `newRuntime` time.
- **`OnInvite` resolves `EntrypointID`** from the in-flight `PhysicalRing` if present, otherwise
  from the single configured entrypoint. If it cannot resolve, respond `486 Busy`: better to reject
  than to create a state without an entrypoint, which `requireDialogAndEntrypoint` would reject
  anyway.
- **`Answer` passes the devaddr to `BuildAnswer`**, which gains the `a=DEVADDR:` line. The
  reference implementation adds this to incoming-call answers with the comment *"incoming calls
  also need a=DEVADDR so the intercom routes RTP correctly"*.
- **`m.active` becomes an interface exposing only `Bye(ctx)`**, so it can hold either the outbound
  dialog or an answered inbound dialog. `Hangup` then works for both without modification.
- **`StartStream`** returns `nil` without sending an INVITE when `m.active` is an answered incoming
  call. It keeps rejecting with `ErrIncomingDialog` while a call is ringing but unanswered, and with
  `ErrActiveDialog` when an outbound preview dialog is already up.
- **`Hangup` becomes idempotent** — see §5.5 for why this is required rather than cosmetic.
- **60-second incoming expiry** (carried over from the v2 implementation): if nobody answers, the
  inbound dialog is closed and state returns to idle, so a lost INVITE cannot leave the card stuck
  on "ringing".
- **`Decline` stays but is not exposed** over HTTP.

### 5.4 State and events — `internal/core`, `internal/openwebnet`

- `internal/openwebnet/mapper.go:61` emits **only `RingStarted`**. `IncomingCallStarted` is removed
  from the mapper, together with the synthetic `openwebnet-<ts>` dialog and the `m.dialog` field.
- On `IsStreamStop` / `IsFreeAVResources` the mapper emits **only `RingCleared`**. `CallHungUp`
  becomes the Manager's responsibility, as it is the only component that knows whether a dialog
  actually exists.
- New event `IncomingCallEnded{DialogID, Reason}` with `Reason ∈ {cancelled, timeout, elsewhere}`.
  `internal/core/state.go:118` already handles the event type; only the reason field and its
  propagation into the DTO are new.

**No regression when `inbound: false`.** Removing `IncomingCallStarted` from the mapper leaves only
`PhysicalRing` populated, and `deriveCallState` treats `PhysicalRing` and `IncomingCall`
identically, so the card still sees `ringing` exactly as today.

### 5.5 API — `internal/api`

| Route | Effect |
| --- | --- |
| `POST /api/v3/call/answer` | `Manager.Answer` only — send `200 OK`. No lease, no source start. |
| `POST /api/v3/call/hangup` | `Manager.Hangup` — BYE the active dialog, or clear a pending incoming one |

Both routes are registered with `handleProtected`, i.e. bearer-authenticated like every other
control route.

A new `CallControl` interface in `internal/api/interfaces.go`, injected via `SetCall` following the
existing setter pattern. When `sip.inbound` is false, `SetCall` is never called and both routes
return `503 unavailable`, matching how `s.entrypoints == nil` and `s.audio == nil` are handled
today.

**Error semantics differ between the two routes, deliberately:**

- `answer` on a call that no longer exists returns **`409 Conflict`** — the user pressed a button
  and deserves to know it did not take effect.
- `hangup` becomes **idempotent**: `Manager.Hangup` returns `nil` when there is neither an active
  nor an incoming dialog, and the route returns `200`. This is required, not cosmetic. When the card
  hangs up it also closes its WebRTC session, which releases the lease and runs
  `SourceSession.Close` → `Manager.Hangup` a second time. A non-idempotent `Hangup` would make
  `SourceSession.Close` return an error on every normal teardown. `ErrNoActiveDialog` consequently
  disappears from the HTTP surface.

`StateDTO` keeps its shape: `IncomingCall` and `ActiveCall` already exist and will now carry the
real SIP Call-ID.

---

## 6. Home Assistant integration (branch `v3`)

Follows existing patterns; no new structures.

- **`const.py`** — `API_PATH_CALL_ANSWER = "/api/v3/call/answer"`,
  `API_PATH_CALL_HANGUP = "/api/v3/call/hangup"`.
- **`api.py`** — `async_call_answer()` and `async_call_hangup()`, modelled on
  `async_unlock_entrypoint`.
- **`websocket_api.py`** — two new commands, `bticino_companion/card_call_answer` and
  `bticino_companion/card_call_hangup`, registered next to the existing four and resolved through
  the same `_camera_for_message`. The card never talks to the companion directly and never sees the
  bearer token, per the v3 card plan's constraint.
- **`models.py` / `camera.py`** — `bticino_call_state` already exists. Two attributes are added:
  - `bticino_can_answer` — true only when the state is `ringing` **and** `incoming_dialog_id` is
    set, i.e. a real SIP dialog exists;
  - `bticino_answered_elsewhere`.

`bticino_can_answer` is what ties the design together: if Flexisip provisioning is missing, no
INVITE arrives, `incoming_dialog_id` stays null and **the Answer button is not rendered**. An
absent button is better than a button that does nothing.

---

## 7. Card

Button logic becomes three-state:

| State | Green | Red |
| --- | --- | --- |
| `idle` | Start live view | hidden |
| `ringing` + `can_answer` | **Answer** | hidden |
| `active` | hidden | **Hang up** |

On Answer the card calls `card_call_answer` and, once confirmed, runs its existing `_start()` — the
same WebRTC path as today, microphone enabled. One tap.

When `answered_elsewhere` arrives the card briefly shows "Answered elsewhere" and returns to idle
without opening a stream.

---

## 8. Installer

`scripts/install.sh` gains a provisioning step which, after remounting the root filesystem
read-write:

1. derives the domain using the same precedence as `DiscoverFlexisipDomain`;
2. **backs up** all three files before touching them;
3. adds `companion@<domain>` to `users.db.txt` (md5 hash copied from an existing line) and to
   `route.conf`, and appends it to the `<sip:alluser@…>` line in `route_int.conf` — **idempotently**:
   if the entry is already present it does nothing;
4. validates that `route_int.conf` is still a single line, and otherwise restores the backup and
   fails loudly;
5. writes `sip.inbound: true` into the companion configuration.

A `--no-sip-inbound` flag allows installing without touching Flexisip.

### 8.1 Accepted limitation

After a logout / login from the DoorEntry app, BTicino rewrites these files and answering stops
working **silently**. Recovery is to re-run the provisioning step. This is a deliberate decision to
keep the runtime free of system-file writes; it must be documented prominently in the installation
README, not only here.

---

## 9. Verification

- **Go unit tests** — `manager_test.go` already has dialog fakes; extend with INVITE → answer →
  hangup, CANCEL during ringing, 60-second expiry, and `StartStream` not sending an INVITE while a
  call is answered. `mapper_test.go` must be updated to assert that `IncomingCallStarted` is no
  longer emitted. `api_test.go` covers both new routes including the `409`.
- **Python tests** — `tests/` already uses plain pytest without importing Home Assistant; cover the
  two client methods and the derivation of `can_answer` / `answered_elsewhere` from the snapshot.
- **On-device verification** — one irreducible manual step: `tcpdump -i lo port 5060` during a real
  ring confirms that the INVITE reaches the companion and that our `200 OK` is accepted. Should the
  answer SDP be rejected, the same capture yields the SDP the real monitor answers with, to mirror.

---

## 10. Error handling

- Answering an already-ended call → `409`; the card returns to idle without a red error.
- `200 OK` rejected by the intercom → close the dialog, return to idle, log at `Error` with the SIP
  status code.
- Media start fails after a successful answer → **the call stays active** (audio is live on the
  physical monitor regardless) and the card surfaces the error over the video area. A streaming
  problem must not hang up a live call.
- `inbound: true` but no INVITE ever received: no detection mechanism, by explicit decision.

---

## 11. Open risk

The answer SDP is modelled on the `c300x-controller` reference (`a=DEVADDR:`, speex/8000 audio,
H264 video, `RTP/SAVP` with a dummy crypto key, inverted tags, echoed `record-route`). If the
intercom rejects it, section 9's packet capture provides the exact SDP to imitate. This is the one
assumption in the design that on-device testing must confirm.

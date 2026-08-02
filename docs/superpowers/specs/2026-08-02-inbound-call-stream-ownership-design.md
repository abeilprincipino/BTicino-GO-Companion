# Inbound Call Stream Ownership — Design

**Date:** 2026-08-02
**Status:** Approved for planning
**Repo:** `BTicinoGO` (branch `feat/sip-incoming-call`)

---

## 1. Problem

With inbound SIP answering wired end to end, the doorbell call now reaches the companion and can be
answered from the card — but the video never appears. The WebRTC session is refused.

Device log, single doorbell press:

```
16:40:01  INF inbound sip invite received   dialog_id=3pOY5Sv2Cm
16:40:03  WRN external stream detected      component=media.coordinator
16:40:17  INF call answered                 component=api
16:40:18  DBG stream lease rejected         entrypoint_id=main reason=external_stream_active
16:40:18  WRN webrtc offer failed           error="media: intercom stream is active externally"
16:40:28  DBG stream lease rejected         entrypoint_id=main reason=external_stream_active
```

The call itself is healthy: it is answered, stays up, and ends with a clean BYE at 16:40:30. Only
the media path fails.

## 2. Root cause

`observeRequestedTrackLocked` (`internal/media/stream_coordinator.go:496-500`) classifies the stream
as externally owned whenever OpenWebNet AV start frames arrive while the companion holds no lease:

```go
if c.leaseID == 0 && c.snapshot.Owner == StreamOwnerIdle {
	c.snapshot.Owner = StreamOwnerExternal
	c.logger.Warn("external stream detected")
}
```

On an inbound call the intercom starts its own AV while ringing — fourteen seconds before the user
answers. `Owner` flips to `External`, and from that moment `reserve()`
(`internal/media/stream_coordinator.go:177-184`) refuses every lease with `ErrExternalStream`.

The heuristic exists so the companion does not fight a third-party consumer that already holds the
intercom. Inbound answering breaks its premise: the "external" stream *is* the call the companion is
about to answer. The feature's own design doc
(`docs/superpowers/specs/2026-08-01-sip-incoming-call-answer-design.md`) never mentions the
coordinator, so this interaction was not considered.

## 3. What already works and must not be rebuilt

The rest of the chain is in place. `signaling.Manager.StartStream`
(`internal/signaling/manager.go:422-434`) already returns without dialing when an answered inbound
call is active:

```go
// The intercom is already streaming for the answered call; a second INVITE
// would come back as 486 Busy Here.
if m.activeIncoming {
	return nil
}
```

So a granted lease drives the existing path unchanged: the factory builds the bridge source,
`SourceSession.Start` skips the INVITE, `av.Start` issues the OpenWebNet AddStream to
`127.0.0.1:5007` and `127.0.0.1:5000` (`internal/openwebnet/av.go:69-73`), and RTP flows into the
receivers. Teardown is likewise already correct and deliberate: releasing the lease runs `Hangup`,
which ends the doorbell call, as documented at `internal/signaling/manager.go:273-278`.

**Only the coordinator's guard is wrong.** No new media source is required.

## 4. Design

The companion takes the stream lazily — at the first real request for media — not eagerly at answer
time.

### 4.1 The predicate

`signaling.Manager` is the only component that knows whether a real dialog exists, and it already
holds the exact fact in `activeIncoming`. It gains a read-only accessor:

```go
// HasAnsweredInboundCall reports whether the active dialog is an inbound call
// the companion has answered.
func (m *Manager) HasAnsweredInboundCall() bool
```

It returns `m.active != nil && m.activeIncoming` under `m.mu`. No new state, no new transitions.

### 4.2 Injection into the coordinator

`media` must not import `signaling`. The dependency is injected as a plain function, in the same
style as the existing `SetStateObserver`:

```go
// SetAnsweredCallProbe supplies the predicate that tells the coordinator whether
// a stream seen as external belongs to a call the companion has answered.
func (c *StreamCoordinator) SetAnsweredCallProbe(probe func() bool)
```

Wiring lives in `internal/app/run.go`, after the RTSP server is constructed:

```go
rtspServer.Coordinator().SetAnsweredCallProbe(calls.HasAnsweredInboundCall)
```

`calls` is created earlier in the same function, so the ordering holds.

A `nil` probe must behave exactly as today. Every existing construction site and test that does not
wire it keeps its current semantics.

### 4.3 The guard, and where the probe is evaluated

`reserve()` refuses an externally owned stream only when it is not ours:

```go
if c.snapshot.Owner == StreamOwnerExternal && !answered {
	// reject with ErrExternalStream, as today
}
```

The probe **must be evaluated before `c.mu` is taken**, not inside the critical section. The probe
acquires `m.mu`; calling it under `c.mu` establishes a `c.mu → m.mu` order that is the reverse of
any path starting in `signaling` and ending in a read of the coordinator snapshot. Evaluating it
outside costs one extra read per acquisition and removes the deadlock class entirely.

The resulting staleness is immaterial. A call cannot go from answered to torn down in the interval
between the two statements without the subsequent teardown observing it; the worst case is a lease
granted for a call that has just ended, which the normal stop path already handles.

The `leaseID != 0` branch is unaffected: a second consumer still gets `ErrStreamBusy` regardless of
the probe. The lease stays exclusive.

### 4.4 Lifecycle after adoption

1. `reserve()` grants the lease: `Owner` becomes `Companion`, `starting` is set, the snapshot resets.
2. The factory builds the bridge source; `SourceSession.Start` runs.
3. `sip.StartStream` is a no-op (`activeIncoming`).
4. `av.Start` issues the AddStream; RTP arrives on 5000/5007; tracks reach `live`.
5. On release, `SourceSession.Close` runs `Hangup`, ending the doorbell call.

Steps 2 to 5 are existing behaviour, exercised today by the outbound preview path.

### 4.5 Accepted cost

Between the answer and the first WebRTC offer, the companion's AV is not running. In the observed
log that window is 1.3 seconds (answer 16:40:17.x, offer 16:40:18.x). During it the visitor speaks
to nobody.

This is not a regression: audio reaches the user only through a WebRTC session, so a call answered
with no session open has no audio path either way. Narrowing the window is a client-side change —
have the card open its offer before calling `/answer` — and is out of scope here.

## 5. Alternatives rejected

**Eager acquisition at answer time.** The lease is exclusive: `reserve()` rejects when
`leaseID != 0`. A lease held by the answer path would make the card's offer fail with
`ErrStreamBusy` — one rejection traded for another. Making it work needs either multi-consumer
leases or a handover protocol with teardown races: a change to the core of the media layer, out of
proportion to the defect. Such a lease would also have no sink, so `watch()` would see tracks that
never flow.

**One-shot `ClearExternal()` at answer time.** Tempting and smaller, but the device log disproves
it: three further `stream control track requested` entries arrive after `call answered` at 16:40:17.
With `leaseID == 0` and `Owner == Idle`, `observeRequestedTrackLocked` flips the owner back to
`External` before the card's offer lands a second later. The condition must be evaluated at
`reserve()` time, not latched once.

## 6. Known risk — foreign AV stop frames

During an inbound call two AV consumers exist on the bus: the intercom's own leg and the companion.
`ObserveControlStop` tears the stream down when `controlLeaseID == leaseID`, and the companion's own
AddStream sets `controlLeaseID`. A stop frame emitted by the *other* consumer afterwards is
therefore attributed to our lease and cuts the video mid-call.

The hazard predates this change, but inbound calls make it far more likely, because this is the only
scenario with two simultaneous consumers.

It is deliberately **not** addressed here. Distinguishing frames by destination may not be possible
within the OpenWebNet AV protocol, and that question deserves its own investigation. Verification on
the device is the test: if the video drops on its own a few seconds into an answered call, this is
the cause and it becomes the next piece of work.

## 7. Testing

**`internal/media/stream_coordinator_test.go`**

- `reserve` rejects with `ErrExternalStream` when `Owner == External` and the probe is `nil`
  (current behaviour, unchanged).
- `reserve` rejects when the probe returns `false`.
- `reserve` grants the lease when the probe returns `true`, and the resulting snapshot reports
  `StreamOwnerCompanion`.
- `reserve` still returns `ErrStreamBusy` when `leaseID != 0`, whatever the probe returns.

**`internal/signaling/manager_test.go`**

- `HasAnsweredInboundCall` is `false` with no call, `false` while ringing, `true` after `Answer`,
  and `false` after `Hangup` and after a remote BYE.

**On-device verification**

Ring, answer from the card, and capture the AV ports:

```sh
tcpdump -i any -n 'udp port 5000 or udp port 5007'
```

Traffic must appear after the answer — it does not today. The companion log must show
`call answered` followed by the stream reaching `live`, and the card must render video.

## 8. Out of scope

Two flexisip provisioning defects were found in the same debugging session and are tracked
separately:

- `provision_flexisip_user` (`scripts/install.sh:250-303`) adds the companion to the `alluser` group
  in `route_int.conf`, but flexisip reads `static-records-file=/etc/flexisip/users/route.conf`. The
  group edit must target `route.conf`.
- The static record written at `scripts/install.sh:270` is `<sip:127.0.0.1>`, which resolves to
  `127.0.0.1:5060` — flexisip itself — instead of the companion's listener. Now that dynamic
  registration supplies a correct contact, this record is redundant and causes flexisip to fork the
  INVITE twice; the companion receives every inbound call in duplicate.

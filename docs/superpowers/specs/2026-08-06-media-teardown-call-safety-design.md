# Media Teardown Must Not End the Call — Design

**Date:** 2026-08-06
**Status:** Approved for planning
**Repo:** `BTicinoGO` (branch `main`)

---

## 1. Problem

Inbound answering and the answered-call stream adoption
(`docs/superpowers/specs/2026-08-02-inbound-call-stream-ownership-design.md`) are deployed. Answering
from the Home Assistant card now shows video — and then the video drops on its own partway through
the call, and the call itself ends.

On-device verification on 2026-08-06 confirmed it. The reported trigger: the doorbell rings both the
physical internal unit and the companion; the user answers from the card; the physical unit keeps
ringing, its leg times out and is closed, and the AV teardown that follows takes the companion's
call with it.

This is the risk recorded in section 6 of the 2026-08-02 spec, now observed rather than predicted.

A second, related defect was recorded as an open check in section 7 of that spec and is also fixed
here: when the doorbell station hangs up, the media lease is left outstanding.

## 2. Root cause

Two independent defects that meet on one damage path.

### 2.1 An OpenWebNet stop frame cannot be attributed to a consumer

The AV protocol is asymmetric. A start frame names its destination; a stop frame names nothing.

```go
// internal/openwebnet/frames.go
BuildAVAddStreamVideo → "*7*300#<ip with # separators>#<port>#<quality>*##"   // has ip and port
FrameStop             = "*7*0*##"                                             // bare constant
FrameFreeAVResources  = "*7*9**##"                                            // bare constant
```

`ObserveControlTrack` (`internal/media/stream_coordinator.go:479-493`) claims the lease for the
observed control stream on *any* start frame that arrives while the lease is starting, without
looking at the destination it carries:

```go
if c.leaseID != 0 {
	if !c.starting { return }
	c.controlLeaseID = c.leaseID
}
```

`ObserveControlStop` (`:499-529`) then sees `c.controlLeaseID == c.leaseID` for the next bare stop,
whoever emitted it, and tears the lease down.

Note that `*7*0*<where>##` is *receive video* addressed to a `where`
(`ParseReceiveVideoWhere`, `frames.go:126-138`), and the stop is that same command with an empty
target. A command with no destination suggests bus-wide semantics — that is, the intercom may be
freeing AV for every consumer, ours included. This design does not depend on which it is; see
section 4.

### 2.2 The damage path: a media teardown ends the SIP call

`stop()` → `finishStop()` → `source.Close(ctx)` → `SourceSession.Close`
(`internal/media/source_session.go:223-233`):

```go
hangup := !s.remoteEnded && !s.terminating
...
if hangup { s.sip.Hangup(ctx) }     // BYE — the doorbell call ends
```

There is a **second, independent** BYE site: the deferred cleanup inside `Start`
(`source_session.go:102-121`), which runs on a failed or cancelled startup:

```go
hangup := sipStarted && !s.remoteEnded && !s.terminating
```

Both sites assume the session owns the dialog. `SourceSession`'s own doc comment states it
(`source_session.go:48`, "SourceSession owns the SIP dialog and both RTP sockets"), and for an
outbound preview it is true. For a lease adopted for an answered inbound call it is false: no INVITE
went out, because `Manager.StartStream` returns early when `activeIncoming`
(`internal/signaling/manager.go:446-450`). The session cannot currently tell the difference —
`SourceSIP.StartStream` returns only an `error` (`source_session.go:16`).

The consequence is broader than the observed symptom. **Every** media-layer misfortune ends the
user's call: a foreign stop frame, an RTP stall reclaimed by `Reconcile`, a managed-source
transport failure, a startup failure, a startup timeout.

### 2.3 A remote BYE leaves the lease outstanding

On a BYE for an answered inbound dialog, `internal/signaling/dialer.go:291-313` notifies only the
manager, deliberately not the media hook:

> Calling it here would tear down media for a call the user is still on.

That was correct before adoption, when no lease could exist during an inbound call. Now a lease can,
and nothing releases it: the card keeps showing the last frame until an OpenWebNet stop frame or the
stall watchdog reclaims it.

## 3. Design — Part 1: media teardown

### 3.1 The session learns whether the dialog is its own

`SourceSIP.StartStream` reports whether it created a dialog:

```go
// StartStream dials the intercom and reports whether this session created the
// SIP dialog. It reports false when an answered inbound call is already up: the
// intercom is streaming for that call, no INVITE goes out, and the dialog
// belongs to the call rather than to this session.
StartStream(context.Context, string) (bool, error)
```

`SourceSession` stores the answer as `ownsDialog`. This is the fact both BYE sites need, and it is
knowable only at start time — which is why classifying teardown reasons alone would not be enough:
the startup-failure site runs from inside `Start`, before any teardown reason exists.

The signature change is contained. `Manager.StartStream` is the only implementation and
`source_session.go:165` its only production call site; the three-argument `dialer.StartStream`
(`manager.go:32`, `:456`) is a different method and is untouched. The rest is test doubles.

### 3.2 Both BYE sites are gated on ownership

```go
// Close
hangup := !s.remoteEnded && !s.terminating && (s.ownsDialog || s.endCallOnClose)

// the deferred cleanup in Start
hangup := sipStarted && s.ownsDialog && !s.remoteEnded && !s.terminating
```

A startup failure is never a user action, so it has no opt-in: it must never end an answered call.

### 3.3 A consumer release still ends the call

Releasing the lease ending the doorbell call is deliberate and documented (section 3 of the
2026-08-02 spec, and `internal/signaling/manager.go:270-278`). It is preserved, and made explicit
rather than incidental.

`stop()` takes a typed cause in place of the bare reason string. The type is unexported, so it adds
nothing to the package's API:

```go
type stopCause struct {
	reason   string // unchanged log value
	endsCall bool
}
```

| Cause | `reason` | `endsCall` |
|---|---|---|
| consumer released the lease | `consumer released` | **true** |
| remote SIP BYE | `remote sip bye` | false |
| OpenWebNet stream stop | `openwebnet stream stop` | false |
| RTP tracks stalled | `rtp tracks stalled` | false |
| managed source failure | `managed source failure` | false |
| source startup failed | `source startup failed` | false |
| source startup timed out | `source startup timed out` | false |

When `endsCall` is true, `finishStop` tells the source before calling `Close`, through an optional
capability interface — the idiom already used by `ManagedSourceBackchannel` and by the
`interface{ SetRemoteDialogEnded(func()) }` assertion in `internal/app/run.go:673`.

**Safe by default.** With this polarity a teardown cause added later cannot end a call; at worst it
leaves a dialog to clean up, which is a nuisance rather than a dropped call. And for an outbound
preview, where `ownsDialog` is true, behaviour is bit-for-bit what it is today.

### 3.4 A remote BYE tears the media down

The `answered` branch of the dialer's BYE handling also calls `d.notifyRemoteDialogEnded()`, reusing
the path that already exists and is exercised by the outbound preview: `source.RemoteDialogEnded()`
sets `remoteEnded` and closes the receivers, then `remoteBYE()` reaches
`stop(lease, remote sip bye)` (`stream_coordinator.go:256-262`). No BYE goes out, because the peer
already sent one.

The manager is then told twice — once by the dialer's own `handler.RemoteDialogEnded()`, once
through the source. That is harmless: `Manager.RemoteDialogEnded` returns early when nothing is
active (`manager.go:328-335`) and only the first call publishes `CallHungUp`. The direct call must
stay, because the source's path notifies the manager only when the session was started.

**Guard.** No new lease state. The fix relies on the exclusivity invariant: `OnInvite` responds busy
while `m.incoming != nil || m.active != nil` (`manager.go:119-123`), so an outbound preview cannot
coexist with an answered inbound call and the BYE cannot reach an unrelated preview's lease. The
dialer comment quoted in 2.3 is rewritten to say why this is now correct, and a test in
`internal/signaling` pins the busy behaviour so that changing it breaks a test rather than this fix.

### 3.5 This reverses a decision the code documents as intentional

`Manager.Hangup` carries a comment that states the present behaviour is on purpose
(`manager.go:270-278`):

> It deliberately has no counterpart to StartStream's no-op on an answered inbound call:
> media.SourceSession.Close runs Hangup on every lease teardown, and that ends a live doorbell call
> on purpose. Without the WebRTC session there is no audio path to the user, so keeping the SIP call
> up would leave the visitor talking to nobody.

The rationale is sound, and this design keeps it — for the case it actually describes. It fails
because it treats two different events as one: the *consumer* going away, and the *media* failing
underneath a consumer that is still there.

When the user closes the card, there is indeed no audio path and no prospect of one, so ending the
call is right. That is `consumer released`, and it still ends the call.

When a foreign stop frame, an RTP stall or a transport failure tears the media down, the user is
mid-conversation. Ending the call does not spare a visitor from talking to nobody — it cuts off the
resident who was talking to them, and makes a condition that would have lasted seconds permanent,
because the card would otherwise re-offer and restore the audio path (section 4). The observed
failure is exactly this case.

So the rule is refined, not discarded: a teardown ends the call when the consumer is done, and does
not when the media merely broke. The comment at `manager.go:270-278` and the ownership claim at
`source_session.go:48` are both rewritten as part of the implementation; leaving them in place would
tell a future reader this change was a mistake.

## 4. What this fixes, and what it does not

It fixes the call dying. It does not keep the video up.

If the internal unit's timeout frees AV bus-wide (the "no destination" reading in 2.1), RTP to the
companion's ports stops and the media teardown is legitimate: the picture goes. What changes is that
the teardown stays inside the media layer instead of emitting a BYE, so the call survives. Recovery
is client-driven and needs nothing new: the card re-offers, the coordinator grants a fresh lease —
the answered-call probe still reports true — and `av.Start` re-issues the AddStream.

If instead the frame only ends the other consumer's leg, the companion's RTP never stops and the
teardown was needless. Avoiding it is a separate improvement, deferred in section 7.

## 5. Alternatives rejected

**Confirming the stop frame against RTP flow.** Treat a stop as a hint: arm it, and on the next
`Reconcile` tick tear down only if no track has received RTP within a confirmation window, otherwise
disarm. This is the right shape for the needless-teardown case, and it is deferred rather than
dismissed. It is not the fix for this defect, for two reasons: it cannot help at all if the AV is
freed bus-wide, and it leaves every other teardown cause — stall, transport failure, startup
failure — still ending the call. It also needs the section 7 measurement to be worth building.

**Attributing the start frame by destination.** Parse the port from `*7*300#...` and claim
`controlLeaseID` only for our own receiver's port. It does not work: our own AddStream is observed
too, so it claims `controlLeaseID` legitimately, and the next bare stop still matches. It would also
require plumbing receiver ports into the coordinator, which today knows nothing about them.

**Requiring both tracks to be flowing** to cancel a pending stop. Re-introduces the same class of
bug through another door: a call whose audio has already died would be ended by a foreign stop
frame, even though a video-only stream is live and the coordinator has a `video_only` health state
for exactly that.

**Ownership alone, with no consumer-release opt-in.** Simpler, but it would leave the doorbell call
up when the user closes the card, contradicting documented behaviour and depending on the card
always calling the hangup endpoint.

**Silencing the internal unit when the card answers.** The most attractive idea of all: remove the
second AV consumer and the whole class of failures disappears. `Manager.Answer` sends only the SIP
200 OK (`manager.go:199`) and no OpenWebNet command, so the intercom is never told the call was
answered elsewhere. But the frame that would tell it is not in the vocabulary the code models, and
guessing is unsafe — if the right frame turned out to be `*7*0*##`, it would stop the companion's
own stream too. It needs the protocol discovery in section 7 and becomes its own work. It is also
not a substitute for this change, which covers causes it cannot reach.

## 6. Testing

**`internal/media/source_session_test.go`**

- `ownsDialog == false`: `Close` sends no `Hangup`, for a teardown that is not a consumer release.
- `ownsDialog == false` plus the consumer-release opt-in: `Close` does send `Hangup`.
- `ownsDialog == false`: a failed startup sends no `Hangup` from the deferred cleanup.
- `ownsDialog == true`: every path behaves as it does today. This is the no-regression anchor for the
  outbound preview.

**`internal/media/stream_coordinator_test.go`**

- `Release` carries a cause with `endsCall` true; every other teardown carries false.
- The existing coverage of `ObserveControlStop` — including the stale-stop case that must stay
  ignored — keeps passing unchanged. This change does not alter when a teardown happens, only
  whether it ends the call.

**`internal/signaling/manager_test.go`**

- `OnInvite` responds busy while a dialog is active. This pins the invariant section 3.4 relies on.

**`internal/signaling/dialer_test.go`**

- A BYE on an answered inbound dialog notifies the media hook as well as the manager.

**On-device verification**

Ring, answer from the card, and let the internal unit time out. The call must stay up. The
companion log must show a teardown whose reason is not `consumer released` and **no** BYE for the
inbound dialog.

Note that the AV capture command in the 2026-08-02 plan is stale: commit d8a7d4b moved the RTP
receivers to ephemeral ports, so `udp port 5000 or udp port 5007` now matches nothing. Use
`tcpdump -i lo -n udp`.

## 7. Deferred, with the measurement that unblocks it

Two questions need one observation session on the device, read from the OpenWebNet trace at
`GET /webui/api/logs/openwebnet` (`internal/webui/server.go:160`):

1. **Does the companion's RTP survive the internal unit's timeout?** Answer from the card, stay in
   the call, keep `tcpdump -i lo -n udp` running, and note which frame appears when the unit times
   out (`*7*0*##` or `*7*9**##`) and whether RTP to the companion's ports stops with it. This decides
   whether confirming the stop against RTP flow (section 5) is worth building.
2. **Is there a frame that says a call was answered elsewhere?** Answer on the physical internal
   unit and capture the frames emitted at that moment. Those are the candidates for the approach in
   section 5 that would remove the second consumer entirely.

Fix the duplicate-fork defect in Part 2 on the test device first, by hand — otherwise every INVITE
arrives twice and pollutes both observations.

## 8. Design — Part 2: flexisip provisioning

Independent of Part 1: it shares only the debugging session that found it. It can be implemented and
committed on its own. These are the two defects recorded in section 8 of the 2026-08-02 spec.

Both live in `provision_flexisip_user` (`scripts/install.sh:250-303`).

**The group edit targets the wrong file.** The companion is added to the `alluser` group in
`route_int.conf` (`install.sh:280`), but flexisip reads
`static-records-file=/etc/flexisip/users/route.conf`. The edit moves to `FLEXISIP_ROUTE`. The
anchored membership check and the validation that follows it — one `alluser` line, contact not
inside a trailing comment (`install.sh:239-248`, `:282-297`) — move with it, where they matter more
because the file is one flexisip actually reads.

On-device evidence that this is the right target: inbound calls reached the companion on 2026-08-06
only after the `alluser` group was corrected by hand in `route.conf`.

**The static record is redundant and harmful.** `install.sh:269-272` writes
`<sip:companion@domain> <sip:127.0.0.1>`, which resolves to flexisip itself rather than the
companion's listener. Dynamic registration now supplies a correct contact, so the record adds
nothing and causes flexisip to fork every inbound INVITE twice. The write is removed, and its
idempotency check goes with it — the anchored `alluser` check on `route.conf` becomes the single
"already provisioned" short-circuit.

**`route_int.conf` is no longer touched**, so it also leaves the required-and-backed-up list at
`install.sh:252`. Demanding a file the installer no longer writes would fail an installation for no
reason.

**No migration.** The installer stops writing the bad record but does not remove records it has
already written. Devices installed before this change keep receiving every INVITE twice until the
line is deleted by hand:

```sh
grep -n '<sip:127\.0\.0\.1>' /etc/flexisip/users/route.conf
sed -i.bak '/<sip:127\.0\.0\.1>/d' /etc/flexisip/users/route.conf
/etc/init.d/flexisip restart
```

**Testing.** `scripts/` has no test harness, so this part is verified by inspection and on the
device: a fresh provisioning run must add the companion to the `alluser` group of `route.conf`,
write no static record, leave `route_int.conf` untouched, and be idempotent on a second run.

## 9. Out of scope

- Confirming the stop frame against RTP flow (section 5), pending measurement 1.
- Telling the intercom that a call was answered elsewhere (section 5), pending measurement 2.
- Removing the bad static record from already-installed devices (section 8).
- Any change to when a media teardown happens. This design changes only what a teardown is allowed
  to do to the SIP call.

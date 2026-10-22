# Shared Intercom Stream and Doorbell Snapshot — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Several devices share one intercom stream, the companion takes a doorbell picture on every SIP ring and Home Assistant notifies with it, and the state no longer flips back to "ringing" after a hangup.

**Architecture:** The stream coordinator keeps one intercom stream but lets several *shareable* consumers (WebRTC sessions, the ring holder) hold it; a per-stream `fanout` copies RTP to them and replays the current H.264 keyframe to late joiners. A `ringHolder` in `internal/app` joins the stream on `IncomingCallStarted` and leaves on answer or end of ring; a stream born as a ringing preview also writes `snapshots/ring/<id>.jpg`, whose time Home Assistant exposes as an image entity. The reducer clears the physical ring on `CallAnswered`.

**Tech Stack:** Go (companion, pion/webrtc v4, pion/rtp), Python Home Assistant custom integration, vanilla JS custom element (card).

**Spec:** `docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md`

## Global Constraints

- Companion work goes on a new branch `feat/shared-ringing-stream` created from the current `HEAD` of `recover/media-teardown-d10407c` (`git switch -c feat/shared-ringing-stream`). Integration work goes on a new branch `feat/shared-ringing-stream` created from `origin/v3` in `D:/Progetti/BTicinoGO-Integration` (`git fetch origin && git switch -c feat/shared-ringing-stream origin/v3`); the local `v3` branch is stale, never use it.
- `internal/media`, `internal/app` and `internal/api` do not build on the Windows host (`internal/media/audiobridge.go` uses POSIX syscalls). Verify them with `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/ ./internal/app/ ./internal/api/` (type-checks tests too) and `GOOS=linux GOARCH=arm GOARM=7 go build ./...`. **Never cross-compile into WSL to run them.** Say plainly in every report that those tests were written but not run.
- `internal/core` builds and tests natively: `go test ./internal/core/`.
- Home Assistant is not installed locally: `tests/test_entities.py` skips. `tests/test_models_protocol.py` runs: `python -m pytest tests/test_models_protocol.py -q`.
- `dist/companion` is modified in the working tree and is not part of this work: never `git add` it. Stage files by explicit path only.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Invariant from the 2026-10-15 spec, still binding: **before `CallAnswered`, no path of a media session sends a BYE or a 603.** `Leave` never ends a call; only the last `Release` does.
- Comments follow the repo's style: explain *why*, reference the spec path when a rule comes from it.

## Deviation from the spec, decided while planning

Spec 4.7 clears the physical ring on `CallAnswered`, `CallDeclined`, `CallHungUp` and `IncomingCallEnded`. The existing test `TestProjector_DeclineClearsOnlyIncomingDialog` makes it a deliberate rule that declining the companion's SIP leg leaves the physical ring alone (the indoor unit may still ring). Only `CallAnswered` clears it: once answered, the ring is over, and the reported second "Ringing" happens only after an answered call. Task 1 updates the spec.

Spec 4.5 says the ring file is `snapshots/<id>-ring.jpg`. The plan uses `snapshots/ring/<id>.jpg`, so an entrypoint named `x-ring` cannot collide with the ring file of `x`. Task 5 updates the spec.

## File map

| File | Change |
|---|---|
| `internal/core/state.go`, `state_test.go` | `CallAnswered` clears the physical ring of its entrypoint |
| `internal/media/fanout.go` (new), `fanout_test.go` (new) | consumer set, RTP fan-out, keyframe cache |
| `internal/media/stream_coordinator.go`, `stream_coordinator_test.go` | shared lease: join, `Release`/`Leave`, every consumer notified; `Handover` removed |
| `internal/media/webrtc.go`, `webrtc_test.go` | no supersede; `Leave(sessionID)` |
| `internal/media/snapshot.go`, `snapshot_test.go` | `MarkRing`, ring file, `LatestRing`, `UpdatedAt` |
| `internal/app/ringholder.go` (new), `ringholder_test.go` (new), `run.go` | ring holder wired into the call events; `bridgeSource` marks ring captures |
| `internal/api/{interfaces,handlers,dto,api,websocket}.go`, `api_test.go` | `ring.jpg`, `*_updated_at`, `close` with `leave` |
| Integration `models.py`, `const.py`, `api.py`, `camera.py`, `websocket_api.py`, `tests/test_models_protocol.py`, `tests/test_entities.py` | timestamps, ring snapshot client, `leave` |
| Integration `image.py` (new), `dynamic_entities.py`, `const.py` (`PLATFORMS`), `README.md`, `tests/test_entities.py` | "Last doorbell ring" image entity |
| Integration `www/bticino-go-intercom-card.js` | answered elsewhere → leave and show a notice |

---

### Task 1: The physical ring ends with the answer

**Files:**
- Modify: `internal/core/state.go` (`case CallAnswered:` in `apply`, around line 135)
- Test: `internal/core/state_test.go`
- Modify: `docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md` (section 4.7)

**Interfaces:**
- Produces: after `CallAnswered`, `State.PhysicalRing` is nil when it belonged to the answered call's entrypoint.

- [ ] **Step 1: Branch**

```bash
cd /d/Progetti/BTicinoGO && git switch -c feat/shared-ringing-stream
```

- [ ] **Step 2: Write the failing test** — append to `internal/core/state_test.go`:

```go
func TestProjector_HangupAfterAnswerGoesIdleNotRinging(t *testing.T) {
	t.Parallel()

	projector := NewProjector()
	applyEvent(t, projector, RingStarted{EntrypointID: testEntrypoint})
	applyEvent(t, projector, IncomingCallStarted{DialogID: testDialogID, EntrypointID: testEntrypoint})
	applyEvent(t, projector, CallAnswered{DialogID: testDialogID})

	if ring := projector.Snapshot().PhysicalRing; ring != nil {
		t.Fatalf("physical ring after the answer = %#v, want nil", ring)
	}

	// The bus stop frame arrives only after the media stops, so the hangup is
	// applied while it is still pending.
	applyEvent(t, projector, CallHungUp{DialogID: testDialogID})

	if state := projector.Snapshot().CallState; state != CallStateIdle {
		t.Fatalf("call state after hangup = %q, want %q", state, CallStateIdle)
	}

	// The late stop frame is a no-op.
	applyEvent(t, projector, RingCleared{EntrypointID: testEntrypoint})

	if state := projector.Snapshot().CallState; state != CallStateIdle {
		t.Fatalf("call state after the stop frame = %q, want %q", state, CallStateIdle)
	}
}

func TestProjector_AnswerKeepsAnotherEntrypointsRing(t *testing.T) {
	t.Parallel()

	projector := NewProjector()
	applyEvent(t, projector, RingStarted{EntrypointID: "side"})
	applyEvent(t, projector, IncomingCallStarted{DialogID: testDialogID, EntrypointID: testEntrypoint})
	applyEvent(t, projector, CallAnswered{DialogID: testDialogID})

	if ring := projector.Snapshot().PhysicalRing; ring == nil || ring.EntrypointID != "side" {
		t.Fatalf("physical ring = %#v, want the side entrypoint's ring kept", ring)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/core/ -run "HangupAfterAnswer|AnswerKeepsAnother" -v`
Expected: `TestProjector_HangupAfterAnswerGoesIdleNotRinging` FAILS with `physical ring after the answer = &core.PhysicalRing{...}, want nil`. The other passes.

- [ ] **Step 4: Implement** — in `internal/core/state.go`, replace the `case CallAnswered:` block with:

```go
	case CallAnswered:
		incoming, err := matchingIncoming(state, event.DialogID)
		if err != nil {
			return err
		}

		// The ring is over once the call is answered. The bus frame that would
		// clear it comes only when the media stops, after the hangup, so without
		// this the state reads "ringing" between the hangup and that frame and
		// Home Assistant notifies a ring that never happened. See
		// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
		if state.PhysicalRing != nil && state.PhysicalRing.EntrypointID == incoming.EntrypointID {
			state.PhysicalRing = nil
		}

		state.LastIncomingCallEnd = nil
		state.ActiveCall = &ActiveCall{DialogID: incoming.DialogID, EntrypointID: incoming.EntrypointID}
		state.IncomingCall = nil
```

- [ ] **Step 5: Update the state-machine test** — in `TestProjector_CallAndPreviewStateMachine`, the expected state after `CallAnswered` (Revision 4) loses its `PhysicalRing` line:

```go
	applyEvent(t, projector, CallAnswered{DialogID: testDialogID})
	assertState(t, projector.Snapshot(), State{
		Revision:      4,
		CallState:     CallStateActive,
		ActiveCall:    &ActiveCall{DialogID: testDialogID, EntrypointID: testEntrypoint},
		PreviewStream: &PreviewStream{StreamID: testPreviewID, EntrypointID: testEntrypoint},
	})
```

- [ ] **Step 6: Run all core tests**

Run: `go test ./internal/core/`
Expected: `ok`

- [ ] **Step 7: Update the spec** — in section 4.7 of `docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md`, replace the first paragraph with:

```markdown
In the reducer, `CallAnswered` also clears `PhysicalRing` when it belongs to the answered call's
entrypoint. Decline, hangup and the end of an incoming call keep today's behaviour: the existing
rule (`TestProjector_DeclineClearsOnlyIncomingDialog`) is that declining the companion's SIP leg
does not end the indoor unit's ring, and the reported defect needs only the answer. A later bus
stop frame finds no ring and its `RingCleared` is a no-op. After a hangup the state goes straight
from `active` to `idle`.
```

- [ ] **Step 8: Commit**

```bash
git add internal/core/state.go internal/core/state_test.go docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md
git commit -m "fix(core): clear the physical ring when the call is answered

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Fan-out with keyframe cache

**Files:**
- Create: `internal/media/fanout.go`
- Test: `internal/media/fanout_test.go`

**Interfaces:**
- Produces (package-private, used by Task 3):
  - `type fanoutConsumer struct { id uint64; events SourceEvents }`
  - `func newFanout() *fanout`
  - `func (f *fanout) add(id uint64, events SourceEvents) bool` — replays the cached keyframe to `events.VideoRTP`, then joins; false once closed.
  - `func (f *fanout) remove(id uint64) (remaining int, found bool)`
  - `func (f *fanout) close()`
  - `func (f *fanout) snapshot() []fanoutConsumer`
  - `func (f *fanout) video(*rtp.Packet)`, `audio(*rtp.Packet)`, `remoteBYE()`, `failed(error)`
  - `func startsKeyframe(payload []byte) bool`
  - `const keyframeCacheLimit = 512 * 1024`

- [ ] **Step 1: Write the failing tests** — create `internal/media/fanout_test.go`:

```go
package media

import (
	"errors"
	"testing"

	"github.com/pion/rtp"
)

func h264Packet(sequence uint16, timestamp uint32, payload ...byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: sequence, Timestamp: timestamp}, Payload: payload}
}

// Payload starts: NAL header byte, NAL type in the low five bits.
var (
	idrSlice     = []byte{0x65, 1}       // type 5
	spsUnit      = []byte{0x67, 1}       // type 7
	ppsUnit      = []byte{0x68, 1}       // type 8
	plainSlice   = []byte{0x41, 1}       // type 1
	fuAIDRStart  = []byte{0x7c, 0x85, 1} // FU-A, start bit, type 5
	fuAIDRMiddle = []byte{0x7c, 0x05, 1} // FU-A, no start bit, type 5
)

func TestStartsKeyframe(t *testing.T) {
	stapA := []byte{0x78, 0x00, 0x02, 0x67, 0x01, 0x00, 0x02, 0x68, 0x01} // STAP-A: SPS, PPS
	stapANoKey := []byte{0x78, 0x00, 0x02, 0x41, 0x01}                     // STAP-A: one slice
	stapATruncated := []byte{0x78, 0x00, 0x09, 0x67}                       // size past the end

	tests := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"IDR", idrSlice, true},
		{"SPS", spsUnit, true},
		{"PPS", ppsUnit, false},
		{"slice", plainSlice, false},
		{"FU-A IDR start", fuAIDRStart, true},
		{"FU-A IDR middle", fuAIDRMiddle, false},
		{"STAP-A with SPS", stapA, true},
		{"STAP-A without key", stapANoKey, false},
		{"STAP-A truncated", stapATruncated, false},
		{"empty", nil, false},
	}

	for _, test := range tests {
		if got := startsKeyframe(test.payload); got != test.want {
			t.Errorf("%s: startsKeyframe() = %t, want %t", test.name, got, test.want)
		}
	}
}

type recordedVideo struct{ sequences []uint16 }

func (r *recordedVideo) events() SourceEvents {
	return SourceEvents{VideoRTP: func(packet *rtp.Packet) { r.sequences = append(r.sequences, packet.SequenceNumber) }}
}

func assertSequences(t *testing.T, name string, got []uint16, want ...uint16) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%s got sequences %v, want %v", name, got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s got sequences %v, want %v", name, got, want)
		}
	}
}

func TestFanoutDeliversToEveryConsumerInJoinOrder(t *testing.T) {
	f := newFanout()

	var order []string

	f.add(1, SourceEvents{AudioRTP: func(*rtp.Packet) { order = append(order, "first") }})
	f.add(2, SourceEvents{AudioRTP: func(*rtp.Packet) { order = append(order, "second") }})

	f.audio(h264Packet(1, 1))

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("delivery order = %v, want [first second]", order)
	}
}

func TestFanoutReplaysTheKeyframeToAJoinerWithoutGaps(t *testing.T) {
	f := newFanout()
	first, late := &recordedVideo{}, &recordedVideo{}

	f.add(1, first.events())
	f.video(h264Packet(1, 50, plainSlice...)) // before any keyframe: not cached
	f.video(h264Packet(2, 100, idrSlice...))
	f.video(h264Packet(3, 100, plainSlice...))

	f.add(2, late.events())
	f.video(h264Packet(4, 200, plainSlice...))

	assertSequences(t, "first consumer", first.sequences, 1, 2, 3, 4)
	assertSequences(t, "late joiner", late.sequences, 2, 3, 4)
}

func TestFanoutKeepsTheParameterSetsOfTheKeyframe(t *testing.T) {
	f := newFanout()
	late := &recordedVideo{}

	f.video(h264Packet(1, 100, spsUnit...))
	f.video(h264Packet(2, 100, ppsUnit...))
	f.video(h264Packet(3, 100, idrSlice...)) // same access unit: no restart

	f.add(1, late.events())

	assertSequences(t, "late joiner", late.sequences, 1, 2, 3)
}

func TestFanoutCacheStartsAgainAtTheNextKeyframe(t *testing.T) {
	f := newFanout()
	late := &recordedVideo{}

	f.video(h264Packet(1, 100, idrSlice...))
	f.video(h264Packet(2, 200, plainSlice...))
	f.video(h264Packet(3, 300, fuAIDRStart...))
	f.video(h264Packet(4, 300, fuAIDRMiddle...))

	f.add(1, late.events())

	assertSequences(t, "late joiner", late.sequences, 3, 4)
}

func TestFanoutDropsTheCachePastItsLimit(t *testing.T) {
	f := newFanout()
	late := &recordedVideo{}

	big := make([]byte, keyframeCacheLimit)
	big[0] = 0x41

	f.video(h264Packet(1, 100, idrSlice...))
	f.video(h264Packet(2, 100, big...))
	f.video(h264Packet(3, 200, plainSlice...)) // still waiting for a keyframe

	f.add(1, late.events())

	assertSequences(t, "late joiner", late.sequences)
}

func TestFanoutCachesClonesNotTheReceiverBuffer(t *testing.T) {
	f := newFanout()
	payload := append([]byte(nil), idrSlice...)

	f.video(h264Packet(1, 100, payload...))
	payload[1] = 0xee // the receiver reuses its read buffer

	var replayed []byte

	f.add(1, SourceEvents{VideoRTP: func(packet *rtp.Packet) { replayed = packet.Payload }})

	if len(replayed) != 2 || replayed[1] != 1 {
		t.Fatalf("replayed payload = %v, want the bytes at caching time", replayed)
	}
}

func TestFanoutRemoveCountsWhoIsLeft(t *testing.T) {
	f := newFanout()
	f.add(1, SourceEvents{})
	f.add(2, SourceEvents{})

	if remaining, found := f.remove(1); !found || remaining != 1 {
		t.Fatalf("remove(1) = %d, %t, want 1, true", remaining, found)
	}

	if _, found := f.remove(1); found {
		t.Fatal("remove(1) twice found the consumer again")
	}

	if remaining, found := f.remove(2); !found || remaining != 0 {
		t.Fatalf("remove(2) = %d, %t, want 0, true", remaining, found)
	}
}

func TestFanoutRefusesAJoinOnceClosedAndStillTellsEveryone(t *testing.T) {
	f := newFanout()

	var told error

	f.add(1, SourceEvents{Failed: func(err error) { told = err }})
	f.close()

	if f.add(2, SourceEvents{}) {
		t.Fatal("add() after close() = true, want false")
	}

	cause := errors.New("ended")
	f.failed(cause)

	if !errors.Is(told, cause) {
		t.Fatalf("consumer told %v, want %v", told, cause)
	}
}

func TestFanoutConsumerMayLeaveFromItsOwnCallback(t *testing.T) {
	f := newFanout()
	f.add(1, SourceEvents{VideoRTP: func(*rtp.Packet) { f.remove(1) }})

	f.video(h264Packet(1, 100, plainSlice...)) // must not deadlock

	if remaining, _ := f.remove(1); remaining != 0 {
		t.Fatalf("remaining = %d, want 0", remaining)
	}
}
```

- [ ] **Step 2: Verify they fail to compile**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/`
Expected: errors `undefined: newFanout`, `undefined: startsKeyframe`, `undefined: keyframeCacheLimit`.

- [ ] **Step 3: Implement** — create `internal/media/fanout.go`:

```go
package media

import (
	"sync"

	"github.com/pion/rtp"
)

// keyframeCacheLimit bounds the video kept for consumers that join a running
// stream. A low-resolution intercom GOP stays far below it; past it the cache
// is dropped and waits for the next keyframe.
const keyframeCacheLimit = 512 * 1024

// fanoutConsumer is one consumer of a shared stream.
type fanoutConsumer struct {
	id     uint64
	events SourceEvents
}

// fanout delivers one stream's media to every consumer that holds it, and
// replays the current H.264 keyframe to a consumer that joins mid-stream so its
// picture appears at once instead of at the intercom's next keyframe. See
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
//
// deliveryMu serialises video delivery with a join's replay: a joiner gets the
// cached packets and then the live ones, with no gap and no overlap in the
// sequence numbers. listMu guards the consumer list alone, and remove takes
// only listMu, so a consumer may leave from inside a media callback.
//
// Callbacks run with deliveryMu held and must not call back into the stream
// coordinator synchronously: the same rule the receiver goroutines already
// impose today.
type fanout struct {
	deliveryMu sync.Mutex
	cache      []*rtp.Packet
	cacheBytes int

	listMu    sync.Mutex
	consumers []fanoutConsumer
	closed    bool
}

func newFanout() *fanout {
	return &fanout{}
}

// add replays the cached keyframe to a consumer and then joins it to the live
// stream. It fails once the fan-out is closed: the stream stopped while the
// consumer was joining.
func (f *fanout) add(id uint64, events SourceEvents) bool {
	f.deliveryMu.Lock()
	defer f.deliveryMu.Unlock()

	if f.isClosed() {
		return false
	}

	if events.VideoRTP != nil {
		for _, packet := range f.cache {
			events.VideoRTP(packet)
		}
	}

	f.listMu.Lock()
	defer f.listMu.Unlock()

	if f.closed {
		return false
	}

	f.consumers = append(f.consumers, fanoutConsumer{id: id, events: events})

	return true
}

// remove detaches a consumer and reports how many are left.
func (f *fanout) remove(id uint64) (remaining int, found bool) {
	f.listMu.Lock()
	defer f.listMu.Unlock()

	for i, consumer := range f.consumers {
		if consumer.id == id {
			// A fresh backing array: a snapshot being iterated keeps its own.
			f.consumers = append(f.consumers[:i:i], f.consumers[i+1:]...)

			return len(f.consumers), true
		}
	}

	return len(f.consumers), false
}

// close stops further joins. The list is kept, so the end of the stream can
// still be told to every consumer on it.
func (f *fanout) close() {
	f.listMu.Lock()
	f.closed = true
	f.listMu.Unlock()
}

func (f *fanout) isClosed() bool {
	f.listMu.Lock()
	defer f.listMu.Unlock()

	return f.closed
}

// snapshot returns the current consumers. The slice is never written in place,
// so it can be iterated without a lock.
func (f *fanout) snapshot() []fanoutConsumer {
	f.listMu.Lock()
	defer f.listMu.Unlock()

	return f.consumers
}

func (f *fanout) video(packet *rtp.Packet) {
	f.deliveryMu.Lock()
	defer f.deliveryMu.Unlock()

	f.remember(packet)

	for _, consumer := range f.snapshot() {
		if consumer.events.VideoRTP != nil {
			consumer.events.VideoRTP(packet)
		}
	}
}

func (f *fanout) audio(packet *rtp.Packet) {
	for _, consumer := range f.snapshot() {
		if consumer.events.AudioRTP != nil {
			consumer.events.AudioRTP(packet)
		}
	}
}

func (f *fanout) remoteBYE() {
	for _, consumer := range f.snapshot() {
		if consumer.events.RemoteBYE != nil {
			consumer.events.RemoteBYE()
		}
	}
}

func (f *fanout) failed(err error) {
	for _, consumer := range f.snapshot() {
		if consumer.events.Failed != nil {
			consumer.events.Failed(err)
		}
	}
}

// remember keeps the video from the latest keyframe onward. SPS, PPS and IDR
// of one access unit share a timestamp, so a keyframe start with the timestamp
// already at the head of the cache continues it rather than restarting it. The
// receiver reuses its read buffer, so the cache holds clones.
func (f *fanout) remember(packet *rtp.Packet) {
	keyframe := startsKeyframe(packet.Payload)
	if keyframe && (len(f.cache) == 0 || f.cache[0].Timestamp != packet.Timestamp) {
		f.cache, f.cacheBytes = f.cache[:0], 0
	}

	if len(f.cache) == 0 && !keyframe {
		return
	}

	if f.cacheBytes+len(packet.Payload) > keyframeCacheLimit {
		f.cache, f.cacheBytes = nil, 0

		return
	}

	f.cache = append(f.cache, packet.Clone())
	f.cacheBytes += len(packet.Payload)
}

// startsKeyframe reports whether an H.264 RTP payload (RFC 6184) begins a
// keyframe: an IDR slice (type 5) or an SPS (type 7), alone, inside a STAP-A,
// or as the first fragment of an FU-A.
func startsKeyframe(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}

	isKey := func(nalHeader byte) bool {
		naluType := nalHeader & 0x1f
		return naluType == 5 || naluType == 7
	}

	switch payload[0] & 0x1f {
	case 24: // STAP-A: 16-bit size, then the unit, repeated.
		for offset := 1; offset+2 < len(payload); {
			size := int(payload[offset])<<8 | int(payload[offset+1])
			if size == 0 || offset+2+size > len(payload) {
				return false
			}

			if isKey(payload[offset+2]) {
				return true
			}

			offset += 2 + size
		}

		return false
	case 28: // FU-A: the FU header carries the start bit and the unit type.
		return len(payload) > 1 && payload[1]&0x80 != 0 && isKey(payload[1])
	default:
		return isKey(payload[0])
	}
}
```

- [ ] **Step 4: Verify it type-checks**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/`
Expected: no output. (Tests not run on this host; see Global Constraints.)

- [ ] **Step 5: Commit**

```bash
git add internal/media/fanout.go internal/media/fanout_test.go
git commit -m "feat(media): fan a stream out to several consumers with a keyframe cache

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Shared lease in the stream coordinator

**Files:**
- Modify: `internal/media/stream_coordinator.go`
- Test: `internal/media/stream_coordinator_test.go`

**Interfaces:**
- Consumes: Task 2's `fanout`.
- Produces:
  - `type StreamLease struct{ id, consumer uint64 }` — one consumer's handle.
  - `var ErrStreamStopped = errors.New("media: stream stopped")`
  - `func (c *StreamCoordinator) Release(lease *StreamLease) bool` — detaches the consumer; the last one out stops the stream with `causeConsumerReleased` (ends the call).
  - `func (c *StreamCoordinator) Leave(lease *StreamLease) bool` — same, but the last one out stops with `causeConsumerLeft` (never ends the call).
  - `AcquireRingingPreview` joins a running stream born from a shareable acquire for the same entrypoint. `Acquire`/`AcquireWithStartup` never join and are never joined.
  - `Handover` and `causeConsumerSuperseded` no longer exist.

- [ ] **Step 1: Write the failing tests** — append to `internal/media/stream_coordinator_test.go`:

```go
// sharedFactory builds one source per stream and records the wrapped events the
// coordinator hands it, so a test can push media into the stream.
type sharedFactory struct {
	mu      sync.Mutex
	sources []ManagedSource
	events  []SourceEvents
	build   func() ManagedSource
}

func (f *sharedFactory) factory() ManagedSourceFactory {
	return func(_ config.Entrypoint, events SourceEvents) (ManagedSource, func(), error) {
		f.mu.Lock()
		defer f.mu.Unlock()

		source := f.build()
		f.sources = append(f.sources, source)
		f.events = append(f.events, events)

		return source, nil, nil
	}
}

func (f *sharedFactory) created() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.sources)
}

func (f *sharedFactory) streamEvents(i int) SourceEvents {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.events[i]
}

func TestStreamCoordinatorShareableAcquireJoinsTheRunningStream(t *testing.T) {
	shared := &sharedFactory{build: func() ManagedSource { return &callEndingSource{} }}
	c := NewStreamCoordinator(nil, shared.factory())

	var first, second []uint16

	firstLease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
		VideoRTP: func(packet *rtp.Packet) { first = append(first, packet.SequenceNumber) },
	})
	if err != nil {
		t.Fatal(err)
	}

	secondLease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
		VideoRTP: func(packet *rtp.Packet) { second = append(second, packet.SequenceNumber) },
	})
	if err != nil {
		t.Fatalf("second AcquireRingingPreview() error = %v, want a join", err)
	}

	if shared.created() != 1 {
		t.Fatalf("sources created = %d, want 1", shared.created())
	}

	if firstLease.id != secondLease.id || firstLease.consumer == secondLease.consumer {
		t.Fatalf("leases = %#v and %#v, want one stream and two consumers", firstLease, secondLease)
	}

	shared.streamEvents(0).VideoRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: 7}, Payload: []byte{0x41}})

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("deliveries first=%v second=%v, want one each", first, second)
	}
}

func TestStreamCoordinatorExclusiveLeasesNeverShare(t *testing.T) {
	c := NewStreamCoordinator(nil, testManagedSourceFactory())

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Acquire(context.Background(), testEntrypoint, SourceEvents{}); !errors.Is(err, ErrStreamBusy) {
		t.Fatalf("Acquire() on a shared stream error = %v, want ErrStreamBusy", err)
	}

	c.Release(lease)

	exclusive, err := c.Acquire(context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{}); !errors.Is(err, ErrStreamBusy) {
		t.Fatalf("AcquireRingingPreview() on an exclusive stream error = %v, want ErrStreamBusy", err)
	}

	c.Release(exclusive)
}

func TestStreamCoordinatorOnlyTheLastReleaseEndsTheCall(t *testing.T) {
	source := &callEndingSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	first, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	second, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if !c.Release(first) {
		t.Fatal("Release(first) = false")
	}

	if source.closes != 0 || source.endCallCalls != 0 {
		t.Fatalf("after a middle release closes=%d endCalls=%d, want 0 and 0", source.closes, source.endCallCalls)
	}

	if c.Release(first) {
		t.Fatal("Release(first) twice = true, want a no-op")
	}

	if !c.Release(second) {
		t.Fatal("Release(second) = false")
	}

	if source.closes != 1 || source.endCallCalls != 1 {
		t.Fatalf("after the last release closes=%d endCalls=%d, want 1 and 1", source.closes, source.endCallCalls)
	}
}

func TestStreamCoordinatorLeaveNeverEndsTheCall(t *testing.T) {
	source := &callEndingSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if !c.Leave(lease) {
		t.Fatal("Leave() = false")
	}

	if source.closes != 1 || source.endCallCalls != 0 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 0", source.closes, source.endCallCalls)
	}

	if c.Snapshot().Owner != StreamOwnerIdle {
		t.Fatalf("owner = %q, want idle", c.Snapshot().Owner)
	}
}

func TestStreamCoordinatorJoinWaitsForTheStartup(t *testing.T) {
	source := newRingingPreviewSource()
	source.startGate = make(chan struct{})
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	type result struct {
		lease *StreamLease
		err   error
	}

	firstDone, secondDone := make(chan result, 1), make(chan result, 1)

	go func() {
		lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
		firstDone <- result{lease, err}
	}()

	waitSignal(t, source.started, "source Start")

	go func() {
		lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
		secondDone <- result{lease, err}
	}()

	select {
	case <-secondDone:
		t.Fatal("the joiner returned before the stream had started")
	case <-time.After(50 * time.Millisecond):
	}

	close(source.startGate)

	first, second := <-firstDone, <-secondDone
	if first.err != nil || second.err != nil {
		t.Fatalf("errors first=%v second=%v", first.err, second.err)
	}

	if first.lease.id != second.lease.id {
		t.Fatalf("leases on different streams: %#v and %#v", first.lease, second.lease)
	}
}

type failingGatedSource struct {
	started chan struct{}
	gate    chan struct{}
}

func (s *failingGatedSource) Start(context.Context) error {
	s.started <- struct{}{}
	<-s.gate

	return errors.New("intercom refused")
}

func (*failingGatedSource) Close(context.Context) error { return nil }

func TestStreamCoordinatorJoinFailsWithTheStartup(t *testing.T) {
	source := &failingGatedSource{started: make(chan struct{}, 1), gate: make(chan struct{})}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	firstDone, secondDone := make(chan error, 1), make(chan error, 1)

	go func() {
		_, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
		firstDone <- err
	}()

	<-source.started

	go func() {
		_, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
		secondDone <- err
	}()

	time.Sleep(20 * time.Millisecond)
	close(source.gate)

	if err := <-firstDone; err == nil {
		t.Fatal("creator error = nil, want the startup failure")
	}

	if err := <-secondDone; !errors.Is(err, ErrStreamStopped) {
		t.Fatalf("joiner error = %v, want ErrStreamStopped", err)
	}
}

func TestStreamCoordinatorStopRingingTellsEveryConsumer(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)

	var told [2]error

	for i := range told {
		if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
			Failed: func(err error) { told[i] = err },
		}); err != nil {
			t.Fatal(err)
		}
	}

	c.StopRinging()

	for i, err := range told {
		if !errors.Is(err, ErrRingingEnded) {
			t.Fatalf("consumer %d told %v, want ErrRingingEnded", i, err)
		}
	}
}

func TestStreamCoordinatorRemoteBYEReachesEveryConsumer(t *testing.T) {
	shared := &sharedFactory{build: func() ManagedSource { return &fakeManagedSource{} }}
	c := NewStreamCoordinator(nil, shared.factory())

	byes := 0

	for range 2 {
		if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
			RemoteBYE: func() { byes++ },
		}); err != nil {
			t.Fatal(err)
		}
	}

	shared.streamEvents(0).RemoteBYE()

	if byes != 2 {
		t.Fatalf("RemoteBYE delivered %d times, want 2", byes)
	}

	if c.Snapshot().Owner != StreamOwnerIdle {
		t.Fatalf("owner = %q, want idle", c.Snapshot().Owner)
	}
}

func TestStreamCoordinatorJoinerGetsTheCachedKeyframe(t *testing.T) {
	shared := &sharedFactory{build: func() ManagedSource { return &fakeManagedSource{} }}
	c := NewStreamCoordinator(nil, shared.factory())

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{}); err != nil {
		t.Fatal(err)
	}

	shared.streamEvents(0).VideoRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: 9, Timestamp: 100}, Payload: []byte{0x65, 1}})

	var replayed []uint16

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
		VideoRTP: func(packet *rtp.Packet) { replayed = append(replayed, packet.SequenceNumber) },
	}); err != nil {
		t.Fatal(err)
	}

	if len(replayed) != 1 || replayed[0] != 9 {
		t.Fatalf("joiner got %v, want the cached IDR [9]", replayed)
	}
}
```

Then **replace** `TestStreamCoordinatorHandoverDoesNotEndTheCall` (it tests the removed `Handover`) with nothing: `TestStreamCoordinatorLeaveNeverEndsTheCall` above covers the same promise.

- [ ] **Step 2: Verify the tests fail to compile**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/`
Expected: `c.Leave undefined`, `unknown field consumer`, `undefined: ErrStreamStopped`.

- [ ] **Step 3: Implement — errors, causes, lease, start signal.** In `internal/media/stream_coordinator.go`:

Add to the `var (...)` error block:

```go
	ErrStreamStopped          = errors.New("media: stream stopped")
```

In the causes block, replace `causeConsumerSuperseded = stopCause{reason: "consumer superseded"}` with:

```go
	causeConsumerLeft       = stopCause{reason: "consumer left"}
```

Replace `type StreamLease struct{ id uint64 }` with:

```go
// StreamLease is one consumer's hold on the stream: id names the stream and
// consumer the holder. Stream-level paths build it with consumer 0, which never
// names a consumer.
type StreamLease struct{ id, consumer uint64 }

// acquireMode says what an acquire may do. A shareable acquire joins a running
// stream born from another shareable acquire instead of failing busy; see
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
type acquireMode struct {
	allowRinging bool
	shareable    bool
}

// streamStart tells consumers joining a stream that its startup is over, and
// how it went.
type streamStart struct {
	done chan struct{}
	once sync.Once
	err  error
}

func newStreamStart() *streamStart {
	return &streamStart{done: make(chan struct{})}
}

func (s *streamStart) finish(err error) {
	s.once.Do(func() {
		s.err = err
		close(s.done)
	})
}

// reservation is what reserve hands acquire: a new stream to start, or a
// running one to join.
type reservation struct {
	lease   *StreamLease
	preview bool
	join    bool
	fanout  *fanout
	start   *streamStart
}
```

- [ ] **Step 4: Implement — coordinator fields.** In `type StreamCoordinator struct`, replace the `leaseEvents`/`consumerEvents` block with:

```go
	// leaseEvents are the current stream's events as wrapped by sourceEvents.
	// Promotion failure goes through them.
	leaseEvents SourceEvents

	// shared marks the current stream as one a shareable acquire may join;
	// fanout delivers its media to every consumer; start tells joiners when its
	// startup is over. See
	// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
	shared       bool
	fanout       *fanout
	start        *streamStart
	nextConsumer uint64
```

- [ ] **Step 5: Implement — acquire paths.** Replace `Acquire`, `AcquireWithStartup`, `AcquireRingingPreview`, `acquire` and `reserve` with:

```go
func (c *StreamCoordinator) Acquire(ctx context.Context, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error) {
	return c.acquire(ctx, ctx, entrypoint, events, acquireMode{})
}

// AcquireWithStartup bounds source initialization without tying the live lease
// to the caller's short-lived startup deadline.
func (c *StreamCoordinator) AcquireWithStartup(lifecycleCtx, startupCtx context.Context, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error) {
	return c.acquire(lifecycleCtx, startupCtx, entrypoint, events, acquireMode{})
}

// AcquireRingingPreview is AcquireWithStartup for the consumers that may also
// take the stream while the inbound call rings: a WebRTC viewer, which can then
// answer on the same session, and the ring holder. The lease starts video only
// and is promoted by PromoteRinging or stopped by StopRinging. Outside a ring it
// is an ordinary lease.
//
// It is also the shareable acquire: it joins a stream another shareable
// consumer of the same entrypoint already holds, so two devices watch together
// instead of the second closing the first. See
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
func (c *StreamCoordinator) AcquireRingingPreview(lifecycleCtx, startupCtx context.Context, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error) {
	return c.acquire(lifecycleCtx, startupCtx, entrypoint, events, acquireMode{allowRinging: true, shareable: true})
}

func (c *StreamCoordinator) acquire(lifecycleCtx, startupCtx context.Context, entrypoint config.Entrypoint, events SourceEvents, mode acquireMode) (*StreamLease, error) {
	r, err := c.reserve(lifecycleCtx, entrypoint, mode)
	if err != nil {
		return nil, err
	}

	if r.join {
		return c.join(lifecycleCtx, startupCtx, r, entrypoint, events)
	}

	lease := r.lease

	// The cache is empty and nothing flows before Start, so this replays
	// nothing. It fails only if the ring ended while the stream was reserved.
	if !r.fanout.add(lease.consumer, events) {
		return nil, ErrStreamStopped
	}

	wrapped := c.sourceEvents(lease, r.fanout)

	source, cleanup, err := c.factory(entrypoint, wrapped)
	if err != nil || source == nil {
		c.finishStop(lease, nil, cleanup, causeSourceCreation)

		if err != nil {
			c.logger.ErrorContext(lifecycleCtx, "media source creation failed", "lease_id", lease.id, "entrypoint_id", entrypoint.ID, "error", err)
			return nil, err
		}

		c.logger.ErrorContext(lifecycleCtx, "media source creation failed", "lease_id", lease.id, "entrypoint_id", entrypoint.ID, "error", "factory returned nil")

		return nil, ErrStreamBusy
	}

	if r.preview {
		ringingSource, ok := source.(ManagedSourceRingingPreview)
		if !ok {
			c.finishStop(lease, source, cleanup, causeSourceCreation)
			c.logger.ErrorContext(lifecycleCtx, "media source creation failed", "lease_id", lease.id, "entrypoint_id", entrypoint.ID, "error", "source cannot serve a ringing preview")

			return nil, ErrStreamBusy
		}

		ringingSource.PrepareRingingPreview()
	}

	if !c.attachSource(lease, source, cleanup, wrapped) {
		return nil, ErrStreamBusy
	}

	if err := c.startSource(lifecycleCtx, startupCtx, lease, source, entrypoint.ID); err != nil {
		return nil, err
	}

	c.finishStart(lease)
	go c.watch(lifecycleCtx, lease)

	return lease, nil
}

// join waits for the stream's startup, within the joiner's own startup budget,
// and adds the consumer to it. A failed startup, or a stream that stops before
// the consumer is in, fails the join.
func (c *StreamCoordinator) join(ctx, startupCtx context.Context, r reservation, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error) {
	select {
	case <-r.start.done:
	case <-startupCtx.Done():
		return nil, startupCtx.Err()
	}

	if r.start.err != nil {
		return nil, r.start.err
	}

	if !r.fanout.add(r.lease.consumer, events) {
		return nil, ErrStreamStopped
	}

	c.logger.InfoContext(ctx, "stream consumer joined", "lease_id", r.lease.id, "consumer_id", r.lease.consumer, "entrypoint_id", entrypoint.ID)

	return r.lease, nil
}

// reserve decides who may take the stream. While the inbound call rings only a
// ringing preview may, whatever the stream owner is: a card that closes and
// reopens its preview mid-ring finds the owner idle, and RTSP must still wait.
// A shareable acquire finding a shareable stream of the same entrypoint joins it.
func (c *StreamCoordinator) reserve(ctx context.Context, entrypoint config.Entrypoint, mode acquireMode) (reservation, error) {
	ringing := c.ringingCall()
	answered := c.answeredCall()

	c.mu.Lock()
	if ringing && !mode.allowRinging {
		c.mu.Unlock()
		c.logger.DebugContext(ctx, "stream lease rejected", "entrypoint_id", entrypoint.ID, "reason", "inbound_call_ringing")

		return reservation{}, ErrRingingCall
	}

	adopted := c.snapshot.Owner == StreamOwnerExternal

	if adopted && !answered && !ringing {
		c.mu.Unlock()
		c.logger.DebugContext(ctx, "stream lease rejected", "entrypoint_id", entrypoint.ID, "reason", "external_stream_active")

		return reservation{}, ErrExternalStream
	}

	if c.leaseID != 0 && mode.shareable && c.shared && !c.stopping && c.snapshot.EntrypointID == entrypoint.ID {
		c.nextConsumer++
		r := reservation{lease: &StreamLease{id: c.leaseID, consumer: c.nextConsumer}, join: true, fanout: c.fanout, start: c.start}
		c.mu.Unlock()
		c.logger.DebugContext(ctx, "stream consumer joining", "lease_id", r.lease.id, "consumer_id", r.lease.consumer, "entrypoint_id", entrypoint.ID)

		return r, nil
	}

	if c.leaseID != 0 || c.factory == nil {
		c.mu.Unlock()
		c.logger.DebugContext(ctx, "stream lease rejected", "entrypoint_id", entrypoint.ID, "reason", "source_unavailable", "owner", c.snapshot.Owner)

		return reservation{}, ErrStreamBusy
	}

	c.nextID++
	c.nextConsumer++
	c.leaseID = c.nextID
	c.controlLeaseID = 0
	c.starting = true
	c.previewRinging = ringing
	c.promotePending = false
	c.shared = mode.shareable
	c.fanout = newFanout()
	c.start = newStreamStart()
	c.snapshot = StreamSnapshot{LeaseID: c.leaseID, Owner: StreamOwnerCompanion, EntrypointID: entrypoint.ID, DevAddr: entrypoint.DevAddr, Health: StreamHealthStarting}
	r := reservation{lease: &StreamLease{id: c.leaseID, consumer: c.nextConsumer}, preview: ringing, fanout: c.fanout, start: c.start}
	c.mu.Unlock()
	c.logger.InfoContext(ctx, "stream lease acquired", "lease_id", r.lease.id, "entrypoint_id", entrypoint.ID, "adopted_external", adopted, "ringing_preview", ringing, "shared", mode.shareable)

	return r, nil
}
```

- [ ] **Step 6: Implement — events and attach.** Replace `sourceEvents` and `attachSource` with:

```go
// sourceEvents wraps the stream's fan-out: every consumer gets the media, and a
// remote BYE or a source failure stops the stream and is told to all of them.
func (c *StreamCoordinator) sourceEvents(lease *StreamLease, f *fanout) SourceEvents {
	return SourceEvents{
		VideoRTP: func(packet *rtp.Packet) {
			c.ObserveRTPPacket(lease, true, packet)
			f.video(packet)
		},
		AudioRTP: func(packet *rtp.Packet) {
			c.ObserveRTPPacket(lease, false, packet)
			f.audio(packet)
		},
		RemoteBYE: func() {
			c.stop(lease, causeRemoteBYE)
			f.remoteBYE()
		},
		Failed: func(err error) {
			c.logger.Error("media source failed", "lease_id", lease.id, "error", err)
			c.stop(lease, causeSourceFailure)
			f.failed(err)
		},
	}
}

func (c *StreamCoordinator) attachSource(lease *StreamLease, source ManagedSource, cleanup func(), wrapped SourceEvents) bool {
	c.mu.Lock()
	if c.leaseID != lease.id {
		c.mu.Unlock()

		_ = source.Close(context.Background())

		if cleanup != nil {
			cleanup()
		}

		return false
	}

	c.source, c.cleanup = source, cleanup
	c.leaseEvents = wrapped
	c.mu.Unlock()

	return true
}
```

- [ ] **Step 7: Implement — startup completion tells joiners.** In `finishStart`, add a `start *streamStart` to the `var (...)` block, set it inside `if c.leaseID == lease.id {` with `start = c.start`, and after `c.mu.Unlock()` add:

```go
	if start != nil {
		start.finish(nil)
	}
```

- [ ] **Step 8: Implement — Release and Leave.** Replace `Release` and `Handover` with:

```go
// Release lets one consumer go. The last one out ends the stream and, with it,
// the SIP call behind it: that is the teardown a consumer that lets go of the
// stream asks for. A consumer that steps aside without ending the call uses
// Leave instead.
func (c *StreamCoordinator) Release(lease *StreamLease) bool {
	return c.leave(lease, causeConsumerReleased)
}

// Leave lets one consumer go without ever ending the call: the last one out
// stops the stream with no BYE. The ring holder leaves this way, and so does a
// card that closes because the call was answered on another device. See
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
func (c *StreamCoordinator) Leave(lease *StreamLease) bool {
	return c.leave(lease, causeConsumerLeft)
}

func (c *StreamCoordinator) leave(lease *StreamLease, cause stopCause) bool {
	if lease == nil {
		return false
	}

	c.mu.Lock()
	f := c.fanout

	if c.leaseID != lease.id || f == nil {
		c.mu.Unlock()
		c.logger.Debug("stream consumer release ignored; lease is no longer active", "lease_id", lease.id, "consumer_id", lease.consumer, "reason", cause.reason)

		return false
	}
	c.mu.Unlock()

	remaining, found := f.remove(lease.consumer)
	if !found {
		return false
	}

	if remaining > 0 {
		c.logger.Info("stream consumer left", "lease_id", lease.id, "consumer_id", lease.consumer, "reason", cause.reason, "remaining", remaining)

		return true
	}

	return c.stop(lease, cause)
}
```

- [ ] **Step 9: Implement — StopRinging tells everyone.** Replace `StopRinging` with:

```go
// StopRinging ends a ringing preview because the ring is over without this
// companion answering: cancelled, timed out, answered elsewhere or rejected. It
// never ends the call, and tells every consumer so each can close its session.
// A no-op on any other lease.
func (c *StreamCoordinator) StopRinging() {
	c.mu.Lock()
	if c.leaseID == 0 || !c.previewRinging {
		c.mu.Unlock()

		return
	}

	lease := &StreamLease{id: c.leaseID}
	f := c.fanout
	c.mu.Unlock()

	if !c.stop(lease, causeRingingEnded) {
		return
	}

	if f != nil {
		f.failed(ErrRingingEnded)
	}
}
```

- [ ] **Step 10: Implement — stop and finishStop.** In `stop`, replace the `previewLost` block and its use:

```go
	// A ringing preview lost to the media layer would otherwise leave its
	// consumers holding dead sessions that the user could still answer on. Told
	// after the teardown so they close their sessions; a card then falls back
	// to answering with a fresh offer, which adopts the answered call.
	// StopRinging notifies for itself, and release or leave never do.
	var lost *fanout
	if c.previewRinging && (cause == causeControlStop || cause == causeTracksStalled) {
		lost = c.fanout
	}

	// No consumer may join a stream that is going away.
	if c.fanout != nil {
		c.fanout.close()
	}
	c.mu.Unlock()
```

and at the end of `stop`, replace the `previewLost` call with:

```go
	if lost != nil {
		lost.failed(fmt.Errorf("%w: %s", ErrRingingPreviewLost, cause.reason))
	}
```

In `finishStop`, replace the reset block and add the joiner signal:

```go
	c.mu.Lock()

	var start *streamStart

	if c.leaseID == lease.id {
		if c.fanout != nil {
			c.fanout.close()
		}

		start = c.start
		c.leaseID = 0
		c.controlLeaseID = 0
		c.starting = false
		c.stopping = false
		c.previewRinging = false
		c.promotePending = false
		c.promoteCancel = nil
		c.leaseEvents = SourceEvents{}
		c.shared = false
		c.fanout = nil
		c.start = nil
		c.snapshot = StreamSnapshot{Owner: StreamOwnerIdle}
	}

	snapshot := c.snapshot
	observer := c.observer
	c.mu.Unlock()

	// A consumer still waiting to join learns the startup is over for good.
	if start != nil {
		start.finish(ErrStreamStopped)
	}
```

(the observer notification and logging that follow stay as they are).

- [ ] **Step 11: Find what still references the removed names**

Run: `grep -rn "consumerEvents\|Handover\|causeConsumerSuperseded" internal/`
Expected: only `internal/media/webrtc.go` (fixed in Task 4). Nothing in `stream_coordinator.go` or its test.

- [ ] **Step 12: Type-check**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/`
Expected: only errors in `webrtc.go` about `s.coordinator.Handover` (Task 4). If that is the only error, proceed; Task 4 makes the package build again. Do not commit a package that does not build: go straight on to Task 4 and commit both together at its end.

---

### Task 4: WebRTC sessions join instead of superseding

**Files:**
- Modify: `internal/media/webrtc.go` (`Offer`, `prepareOffer`, `createOfferAnswer`, `closeSessionWith` comment; new `Leave`)
- Test: `internal/media/webrtc_test.go`

**Interfaces:**
- Consumes: Task 3's `Release`, `Leave`, joining `AcquireRingingPreview`.
- Produces: `func (s *WebRTCService) Leave(sessionID string) error` — idempotent; the session's consumer leaves without ending the call.

- [ ] **Step 1: Replace the supersede tests** — in `internal/media/webrtc_test.go`, delete `TestWebRTCServiceOfferReplacesPreviousSessionForEntrypoint` and `TestWebRTCServiceSupersededSessionDoesNotEndTheCall`, and add:

```go
func TestWebRTCServiceSecondOfferJoinsTheStream(t *testing.T) {
	source := &callEndingSource{}
	created := 0
	coordinator := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		created++
		return source, nil, nil
	})
	service := newTestWebRTCService(t, coordinator, []config.Entrypoint{{ID: "main", Capabilities: config.Capabilities{Stream: true}}})

	firstOffer, firstClient := testWebRTCOffer(t)
	defer firstClient.Close()

	if _, err := service.Offer(context.Background(), "session-1", "main", firstOffer, nil, nil); err != nil {
		t.Fatalf("offer first session: %v", err)
	}

	secondOffer, secondClient := testWebRTCOffer(t)
	defer secondClient.Close()

	if _, err := service.Offer(context.Background(), "session-2", "main", secondOffer, nil, nil); err != nil {
		t.Fatalf("offer second session: %v", err)
	}

	service.mu.Lock()
	_, firstExists := service.sessions["session-1"]
	_, secondExists := service.sessions["session-2"]
	service.mu.Unlock()

	if !firstExists || !secondExists || created != 1 {
		t.Fatalf("first=%t second=%t sources=%d, want both sessions on one source", firstExists, secondExists, created)
	}

	if err := service.Close("session-1"); err != nil {
		t.Fatal(err)
	}

	if source.closes != 0 {
		t.Fatalf("closes after the first close = %d, want 0 — another device still watches", source.closes)
	}

	if err := service.Close("session-2"); err != nil {
		t.Fatal(err)
	}

	if source.closes != 1 || source.endCallCalls != 1 {
		t.Fatalf("after the last close closes=%d endCalls=%d, want 1 and 1", source.closes, source.endCallCalls)
	}
}

func TestWebRTCServiceLeaveDoesNotEndTheCall(t *testing.T) {
	source := &callEndingSource{}
	coordinator := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})
	service := newTestWebRTCService(t, coordinator, []config.Entrypoint{{ID: "main", Capabilities: config.Capabilities{Stream: true}}})

	offer, client := testWebRTCOffer(t)
	defer client.Close()

	if _, err := service.Offer(context.Background(), "session-1", "main", offer, nil, nil); err != nil {
		t.Fatal(err)
	}

	if err := service.Leave("session-1"); err != nil {
		t.Fatal(err)
	}

	if err := service.Leave("session-1"); err != nil {
		t.Fatalf("second Leave() error = %v, want idempotent", err)
	}

	if source.closes != 1 || source.endCallCalls != 0 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 0", source.closes, source.endCallCalls)
	}

	if err := service.Leave(" "); !errors.Is(err, ErrSessionIDRequired) {
		t.Fatalf("Leave(blank) error = %v, want ErrSessionIDRequired", err)
	}
}
```

- [ ] **Step 2: Implement — no supersede.** In `internal/media/webrtc.go`:

Replace `Offer` with:

```go
// Offer creates an answer that includes every gathered local ICE candidate.
// A second session for the same entrypoint joins the stream the first one
// holds: two devices watch together. See
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
func (s *WebRTCService) Offer(ctx context.Context, sessionID, entrypointID, offerSDP string, iceServers []ICEServer, onLocalCandidate func(*ICECandidate)) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	entrypointID = strings.TrimSpace(entrypointID)
	offerSDP = canonicalizeSDP(offerSDP)

	if err := validateOffer(sessionID, entrypointID, offerSDP, s.coordinator); err != nil {
		return "", err
	}

	s.offerMu.Lock()
	defer s.offerMu.Unlock()

	entrypoint, err := s.prepareOffer(sessionID, entrypointID)
	if err != nil {
		return "", err
	}

	session, err := s.newOfferSession(sessionID, entrypointID, iceServers, onLocalCandidate)
	if err != nil {
		return "", err
	}

	if err := s.registerSession(session); err != nil {
		_ = session.pc.Close()
		return "", err
	}

	if err := s.startOfferSource(ctx, session, entrypoint, offerSDP); err != nil {
		return "", err
	}

	return s.createOfferAnswer(ctx, session, sessionID, entrypointID)
}
```

Replace `prepareOffer` with:

```go
func (s *WebRTCService) prepareOffer(sessionID, entrypointID string) (config.Entrypoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return config.Entrypoint{}, ErrWebRTCClosed
	}

	s.prunePendingCandidatesLocked(time.Now())

	entrypoint, found := s.entrypoints[entrypointID]
	if !found {
		return config.Entrypoint{}, ErrEntrypointNotFound
	}

	if _, exists := s.sessions[sessionID]; exists {
		return config.Entrypoint{}, ErrSessionExists
	}

	return entrypoint, nil
}
```

In `createOfferAnswer`, drop the `replacedSessions int` parameter and the `"replaced_sessions", replacedSessions,` pair from its `"webrtc offer accepted"` log line.

Replace the comment on `closeSessionWith` with:

```go
// closeSessionWith is the one close implementation. dropLease says what happens
// to the session's hold on the stream: Release for a consumer that lets go,
// Leave for one that steps aside without ending the call.
```

Add after `Close`:

```go
// Leave closes a session whose card stepped aside because the call was answered
// on another device: its consumer leaves the stream without ending the call. It
// is idempotent like Close. See
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
func (s *WebRTCService) Leave(sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return ErrSessionIDRequired
	}

	s.closeSessionWith(sessionID, "client left", s.coordinator.Leave)

	return nil
}
```

- [ ] **Step 3: Type-check**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/ && GOOS=linux GOARCH=arm GOARM=7 go build ./...`
Expected: no output. (`internal/app` and `internal/api` do not call `Handover`; if `go build` reports one, replace it with `Leave`.)

- [ ] **Step 4: Commit Tasks 3 and 4 together**

```bash
git add internal/media/stream_coordinator.go internal/media/stream_coordinator_test.go internal/media/webrtc.go internal/media/webrtc_test.go
git commit -m "feat(media): share the intercom stream between WebRTC viewers

A second offer for the same entrypoint joins the running stream instead of
superseding the first session. The last Release ends the call; Leave never does.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Doorbell snapshot file

**Files:**
- Modify: `internal/media/snapshot.go`
- Test: `internal/media/snapshot_test.go`
- Modify: `docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md` (section 4.5)

**Interfaces:**
- Produces:
  - `func (a *snapshotAttempt) MarkRing()` — call before packets flow; the capture then also writes the ring file.
  - `func (s *SnapshotManager) LatestRing(entrypointID string) ([]byte, error)` — same errors as `Latest`.
  - `func (s *SnapshotManager) UpdatedAt(entrypointID string) (latest, ring time.Time)` — zero when absent.
  - Ring file: `<dir>/snapshots/ring/<id>.jpg`.

- [ ] **Step 1: Write the failing tests** — append to `internal/media/snapshot_test.go`:

```go
func waitForRingSnapshot(t *testing.T, manager *SnapshotManager, entrypointID string) []byte {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if image, err := manager.LatestRing(entrypointID); err == nil {
			return image
		}

		time.Sleep(snapshotPollInterval)
	}

	t.Fatal("ring snapshot was not published")

	return nil
}

func TestSnapshotManagerWritesTheRingFileOnlyForARingCapture(t *testing.T) {
	image := []byte{0xff, 0xd8, 7, 0xff, 0xd9}
	manager := newSnapshotManager(t.TempDir(), nil, snapshotRunnerFunc(func(_ context.Context, output string) (snapshotProcess, error) {
		return &fakeSnapshotProcess{}, os.WriteFile(output, image, 0o600)
	}))

	if latest, ring := manager.UpdatedAt("front-door"); !latest.IsZero() || !ring.IsZero() {
		t.Fatalf("UpdatedAt() before any capture = %v, %v, want zero", latest, ring)
	}

	manager.Arm("front-door").Consume(testRTPPacket(1))
	waitForSnapshot(t, manager, "front-door")

	if _, err := manager.LatestRing("front-door"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("LatestRing() after a live-view capture error = %v, want ErrSnapshotNotFound", err)
	}

	attempt := manager.Arm("front-door")
	attempt.MarkRing()
	attempt.Consume(testRTPPacket(2))

	if got := waitForRingSnapshot(t, manager, "front-door"); string(got) != string(image) {
		t.Fatalf("LatestRing() = %v, want %v", got, image)
	}

	if latest, ring := manager.UpdatedAt("front-door"); latest.IsZero() || ring.IsZero() {
		t.Fatalf("UpdatedAt() after a ring capture = %v, %v, want both set", latest, ring)
	}

	if path, ok := manager.ringPath("front-door"); !ok || filepath.Base(filepath.Dir(path)) != "ring" {
		t.Fatalf("ringPath() = %q, %v, want a file under snapshots/ring", path, ok)
	}
}
```

- [ ] **Step 2: Verify they fail to compile**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/`
Expected: `manager.UpdatedAt undefined`, `attempt.MarkRing undefined`, `manager.LatestRing undefined`, `manager.ringPath undefined`.

- [ ] **Step 3: Implement.** In `internal/media/snapshot.go`:

Add `"sync/atomic"` to the imports. Add a field to `snapshotAttempt`:

```go
	// ring marks a capture from a stream born as a ringing preview: its picture
	// is also the doorbell picture.
	ring atomic.Bool
```

Add after `Close`:

```go
// MarkRing makes this capture the doorbell picture too. It is called before the
// stream starts, so it is set before the first packet arrives.
func (a *snapshotAttempt) MarkRing() {
	if a != nil {
		a.ring.Store(true)
	}
}
```

Replace `Latest` with:

```go
// Latest returns the last successful JPEG without triggering media activity.
func (s *SnapshotManager) Latest(entrypointID string) ([]byte, error) {
	return s.read(entrypointID, s.path)
}

// LatestRing returns the picture taken at the last doorbell ring. A live view
// never writes it, so its time tells Home Assistant a new ring happened. See
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
func (s *SnapshotManager) LatestRing(entrypointID string) ([]byte, error) {
	return s.read(entrypointID, s.ringPath)
}

// UpdatedAt returns when the latest and the latest doorbell snapshot were
// taken, zero when there is none. It reads the files' modification times, so it
// survives a restart.
func (s *SnapshotManager) UpdatedAt(entrypointID string) (latest, ring time.Time) {
	if s == nil || s.dir == "" {
		return time.Time{}, time.Time{}
	}

	return modTime(s.path(entrypointID)), modTime(s.ringPath(entrypointID))
}

func (s *SnapshotManager) read(entrypointID string, locate func(string) (string, bool)) ([]byte, error) {
	if s == nil || s.dir == "" {
		return nil, ErrSnapshotUnavailable
	}

	path, ok := locate(entrypointID)
	if !ok {
		return nil, ErrSnapshotUnavailable
	}

	image, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrSnapshotNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("read latest snapshot: %w", err)
	}

	if _, ok := firstJPEG(image); !ok {
		return nil, ErrSnapshotNotFound
	}

	return image, nil
}

func modTime(path string, ok bool) time.Time {
	if !ok {
		return time.Time{}
	}

	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}

	return info.ModTime()
}
```

Replace `publish` with:

```go
func (s *SnapshotManager) publish(attempt *snapshotAttempt, image []byte) bool {
	s.mu.Lock()
	if s.active != attempt {
		s.mu.Unlock()
		return false
	}

	path, ok := s.path(attempt.entrypointID)
	if !ok {
		s.mu.Unlock()
		return false
	}

	if err := writeSnapshot(path, image); err != nil {
		s.mu.Unlock()
		s.logger.Warn("publish snapshot", "error", err)

		return false
	}

	// The doorbell picture has its own file, written only by a stream born as a
	// ringing preview, so a live view never looks like a new ring.
	if attempt.ring.Load() {
		if ringPath, ok := s.ringPath(attempt.entrypointID); ok {
			if err := writeSnapshot(ringPath, image); err != nil {
				s.logger.Warn("publish ring snapshot", "error", err)
			}
		}
	}

	callback := s.onCaptured
	s.mu.Unlock()

	if callback != nil {
		callback()
	}

	return true
}

// writeSnapshot publishes image at path atomically: a reader sees the old file
// or the new one, never a partial JPEG.
func writeSnapshot(path string, image []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*.jpg")
	if err != nil {
		return fmt.Errorf("create snapshot file: %w", err)
	}

	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck // removed after successful rename too

	if _, err := temporary.Write(image); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write snapshot: %w", err)
	}

	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close snapshot: %w", err)
	}

	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("rename snapshot: %w", err)
	}

	return nil
}
```

Add after `path`:

```go
// ringPath is the doorbell picture of an entrypoint. It lives in its own
// directory so no entrypoint name can collide with another's ring file.
func (s *SnapshotManager) ringPath(entrypointID string) (string, bool) {
	path, ok := s.path(entrypointID)
	if !ok {
		return "", false
	}

	return filepath.Join(filepath.Dir(path), "ring", filepath.Base(path)), true
}
```

- [ ] **Step 4: Update the spec** — in section 4.5, replace `snapshots/<id>-ring.jpg` with `snapshots/ring/<id>.jpg` and add after that bullet: `A directory of its own, so an entrypoint named "x-ring" cannot collide with the ring file of "x".`

- [ ] **Step 5: Type-check**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add internal/media/snapshot.go internal/media/snapshot_test.go docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md
git commit -m "feat(media): keep the doorbell picture in a file of its own

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Ring holder and ring-marked captures

**Files:**
- Create: `internal/app/ringholder.go`
- Test: `internal/app/ringholder_test.go`
- Modify: `internal/app/run.go` (`SetEvents` line ~137; `newBridgeSource`; `bridgeSource` and its `PrepareRingingPreview`)

**Interfaces:**
- Consumes: `media.StreamCoordinator.AcquireRingingPreview`, `Leave` (Task 3); `snapshotAttempt.MarkRing` (Task 5).
- Produces:
  - `func newRingHolder(coordinator ringHolderCoordinator, entrypoints func() []config.Entrypoint, logger *slog.Logger) *ringHolder`
  - `func (h *ringHolder) route(next func(core.Event)) func(core.Event)`

- [ ] **Step 1: Write the failing tests** — create `internal/app/ringholder_test.go`:

```go
package app

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

type fakeHolderCoordinator struct {
	acquired chan config.Entrypoint
	left     chan *media.StreamLease
	gate     chan struct{}
	err      error
}

func newFakeHolderCoordinator() *fakeHolderCoordinator {
	return &fakeHolderCoordinator{acquired: make(chan config.Entrypoint, 4), left: make(chan *media.StreamLease, 4)}
}

func (f *fakeHolderCoordinator) AcquireRingingPreview(_, _ context.Context, entrypoint config.Entrypoint, _ media.SourceEvents) (*media.StreamLease, error) {
	if f.gate != nil {
		<-f.gate
	}

	f.acquired <- entrypoint

	if f.err != nil {
		return nil, f.err
	}

	return &media.StreamLease{}, nil
}

func (f *fakeHolderCoordinator) Leave(lease *media.StreamLease) bool {
	f.left <- lease
	return true
}

var holderEntrypoints = []config.Entrypoint{
	{ID: "main", Capabilities: config.Capabilities{Stream: true}},
	{ID: "gate", Capabilities: config.Capabilities{Unlock: true}},
}

func newTestRingHolder(coordinator *fakeHolderCoordinator) *ringHolder {
	return newRingHolder(coordinator, func() []config.Entrypoint { return holderEntrypoints }, slog.New(slog.DiscardHandler))
}

func waitLeft(t *testing.T, left <-chan *media.StreamLease) *media.StreamLease {
	t.Helper()

	select {
	case lease := <-left:
		return lease
	case <-time.After(2 * time.Second):
		t.Fatal("the ring holder never left the stream")
		return nil
	}
}

func TestRingHolderHoldsTheStreamUntilTheAnswer(t *testing.T) {
	coordinator := newFakeHolderCoordinator()

	var applied []core.Event

	route := newTestRingHolder(coordinator).route(func(event core.Event) { applied = append(applied, event) })

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})

	select {
	case entrypoint := <-coordinator.acquired:
		if entrypoint.ID != "main" {
			t.Fatalf("acquired entrypoint %q, want main", entrypoint.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the ring holder never acquired the stream")
	}

	// The acquire runs on its own goroutine: give it time to store the lease.
	time.Sleep(20 * time.Millisecond)

	route(core.CallAnswered{DialogID: "d"})

	if waitLeft(t, coordinator.left) == nil {
		t.Fatal("left a nil lease")
	}

	if len(applied) != 2 {
		t.Fatalf("applied %d events, want 2 — every event still reaches the projector", len(applied))
	}
}

func TestRingHolderLeavesAStreamThatStartsAfterTheRingEnded(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	coordinator.gate = make(chan struct{})
	route := newTestRingHolder(coordinator).route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})
	route(core.IncomingCallEnded{DialogID: "d", Reason: core.CallEndReasonCancelled})

	close(coordinator.gate)

	if waitLeft(t, coordinator.left) == nil {
		t.Fatal("left a nil lease")
	}
}

func TestRingHolderIgnoresAnEntrypointWithoutStream(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	route := newTestRingHolder(coordinator).route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "gate"})

	select {
	case <-coordinator.acquired:
		t.Fatal("acquired a stream for an entrypoint that cannot stream")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRingHolderSurvivesAFailedAcquire(t *testing.T) {
	coordinator := newFakeHolderCoordinator()
	coordinator.err = errors.New("busy")
	route := newTestRingHolder(coordinator).route(func(core.Event) {})

	route(core.IncomingCallStarted{DialogID: "d", EntrypointID: "main"})
	<-coordinator.acquired
	time.Sleep(20 * time.Millisecond)

	route(core.CallHungUp{DialogID: "d"})

	select {
	case <-coordinator.left:
		t.Fatal("left a stream it never held")
	case <-time.After(50 * time.Millisecond):
	}
}
```

- [ ] **Step 2: Verify they fail to compile**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/app/`
Expected: `undefined: newRingHolder`.

- [ ] **Step 3: Implement** — create `internal/app/ringholder.go`:

```go
package app

import (
	"bticino-go-companion/internal/config"
	"bticino-go-companion/internal/core"
	"bticino-go-companion/internal/media"
	"context"
	"log/slog"
	"sync"
	"time"
)

// ringHolderStartupTimeout bounds the stream startup the holder waits for: the
// same budget a card's offer gets.
const ringHolderStartupTimeout = 15 * time.Second

// ringHolderCoordinator is the part of the stream coordinator the ring holder
// uses.
type ringHolderCoordinator interface {
	AcquireRingingPreview(lifecycleCtx, startupCtx context.Context, entrypoint config.Entrypoint, events media.SourceEvents) (*media.StreamLease, error)
	Leave(*media.StreamLease) bool
}

// ringHolder opens the intercom stream on every SIP ring. The companion
// receives video only when it asks for it, so without this no doorbell picture
// is taken while no card is open. It keeps the stream until the answer or the
// end of the ring, so a card opening later joins it rather than asking the
// intercom for video on a new port mid-ring. See
// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
type ringHolder struct {
	coordinator ringHolderCoordinator
	entrypoints func() []config.Entrypoint
	logger      *slog.Logger

	mu sync.Mutex
	// generation changes on every ring and every end of one, so an acquire
	// that completes late knows its ring is gone.
	generation uint64
	lease      *media.StreamLease
}

func newRingHolder(coordinator ringHolderCoordinator, entrypoints func() []config.Entrypoint, logger *slog.Logger) *ringHolder {
	return &ringHolder{coordinator: coordinator, entrypoints: entrypoints, logger: logger.With("component", "ring_holder")}
}

// route follows the call events and passes every event on to next. The answer
// is seen after PromoteRinging when routeRingingPreview wraps this, so a card
// on the stream is promoted before the holder steps away.
func (h *ringHolder) route(next func(core.Event)) func(core.Event) {
	return func(event core.Event) {
		switch event := event.(type) {
		case core.IncomingCallStarted:
			h.hold(event.EntrypointID)
		case core.CallAnswered, core.IncomingCallEnded, core.CallDeclined, core.CallHungUp:
			h.release()
		}

		next(event)
	}
}

func (h *ringHolder) hold(entrypointID core.EntrypointID) {
	h.mu.Lock()
	h.generation++
	generation := h.generation
	previous := h.lease
	h.lease = nil
	h.mu.Unlock()

	if previous != nil {
		h.coordinator.Leave(previous)
	}

	entrypoint, ok := h.entrypoint(entrypointID)
	if !ok {
		h.logger.Debug("ring holder skipped; the entrypoint cannot stream", "entrypoint_id", entrypointID)
		return
	}

	go h.acquire(generation, entrypoint)
}

func (h *ringHolder) acquire(generation uint64, entrypoint config.Entrypoint) {
	startupCtx, cancel := context.WithTimeout(context.Background(), ringHolderStartupTimeout)
	defer cancel()

	lease, err := h.coordinator.AcquireRingingPreview(context.Background(), startupCtx, entrypoint, media.SourceEvents{})
	if err != nil {
		h.logger.Warn("ring holder could not open the stream; no doorbell picture for this ring", "entrypoint_id", entrypoint.ID, "error", err)
		return
	}

	h.mu.Lock()
	current := h.generation == generation
	if current {
		h.lease = lease
	}
	h.mu.Unlock()

	// The ring was answered or ended while the stream started.
	if !current {
		h.coordinator.Leave(lease)
	}
}

func (h *ringHolder) release() {
	h.mu.Lock()
	h.generation++
	lease := h.lease
	h.lease = nil
	h.mu.Unlock()

	if lease != nil {
		h.coordinator.Leave(lease)
	}
}

func (h *ringHolder) entrypoint(id core.EntrypointID) (config.Entrypoint, bool) {
	for _, entrypoint := range h.entrypoints() {
		if core.EntrypointID(entrypoint.ID) == id && entrypoint.Capabilities.Stream {
			return entrypoint, true
		}
	}

	return config.Entrypoint{}, false
}
```

- [ ] **Step 4: Wire it** — in `internal/app/run.go`, replace

```go
	runtime.calls.SetEvents(eventSinkFunc(routeRingingPreview(runtime.rtspServer.Coordinator(), applyEvent)))
```

with

```go
	// The ring holder sits inside routeRingingPreview, so on the answer the
	// preview is promoted before the holder leaves the stream. See
	// docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
	holder := newRingHolder(runtime.rtspServer.Coordinator(), func() []config.Entrypoint { return configStore.Snapshot().Companion.Entrypoints }, logger)
	runtime.calls.SetEvents(eventSinkFunc(routeRingingPreview(runtime.rtspServer.Coordinator(), holder.route(applyEvent))))
```

(keep the long comment above the original line in place, above both new lines).

- [ ] **Step 5: Mark ring captures** — in `internal/app/run.go`:

In `newBridgeSource`, change the return to set the new field:

```go
	return &bridgeSource{source: source, bridge: bridge, markRing: func() {
		if attempt != nil {
			attempt.MarkRing()
		}
	}}, func() {
```

Add the field to `bridgeSource`:

```go
type bridgeSource struct {
	source *media.SourceSession
	bridge *media.AudioBridge
	// markRing makes the snapshot taken from this source the doorbell picture.
	markRing func()
}
```

Replace `bridgeSource.PrepareRingingPreview` with:

```go
func (s *bridgeSource) PrepareRingingPreview() {
	// A stream born as a ringing preview is the doorbell's: its picture is the
	// one Home Assistant notifies with.
	if s.markRing != nil {
		s.markRing()
	}

	s.source.PrepareRingingPreview()
}
```

- [ ] **Step 6: Type-check and build**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/app/ && GOOS=linux GOARCH=arm GOARM=7 go build ./...`
Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add internal/app/ringholder.go internal/app/ringholder_test.go internal/app/run.go
git commit -m "feat(app): open the stream on every SIP ring for the doorbell picture

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: API — ring picture, snapshot times, leaving a session

**Files:**
- Modify: `internal/api/interfaces.go` (`SnapshotControl`, `WebRTCControl`), `internal/api/handlers.go` (`snapshotLatest`), `internal/api/api.go` (route, `currentPayload`), `internal/api/dto.go` (`EntrypointDTO`), `internal/api/websocket.go` (`webrtcClosePayload`, `case "close"`)
- Test: `internal/api/api_test.go`

**Interfaces:**
- Consumes: `SnapshotManager.LatestRing`, `UpdatedAt` (Task 5); `WebRTCService.Leave` (Task 4).
- Produces (wire contract, used by the integration):
  - `GET /api/v3/entrypoints/{id}/snapshot/ring.jpg` — `200 image/jpeg`, `404 snapshot_not_found`.
  - State entrypoint fields `snapshot_updated_at`, `ring_snapshot_updated_at` — RFC 3339 UTC, millisecond precision, omitted when absent.
  - WebRTC websocket `close` payload `{"session_id": "...", "reason": "...", "leave": true}`.

- [ ] **Step 1: Write the failing tests** — in `internal/api/api_test.go`:

Replace `snapshotRecorder` and its method with:

```go
type snapshotRecorder struct {
	image, ring      []byte
	err              error
	latestAt, ringAt time.Time
}

func (r snapshotRecorder) Latest(string) ([]byte, error)     { return r.image, r.err }
func (r snapshotRecorder) LatestRing(string) ([]byte, error) { return r.ring, r.err }
func (r snapshotRecorder) UpdatedAt(string) (time.Time, time.Time) {
	return r.latestAt, r.ringAt
}
```

Add to `webRTCRecorder` a field `left string` and the method:

```go
func (r *webRTCRecorder) Leave(sessionID string) error {
	r.left = sessionID
	return nil
}
```

Append the tests:

```go
func TestServer_SnapshotRing(t *testing.T) {
	server, _ := newTestServer(t)
	server.SetSnapshot(snapshotRecorder{ring: []byte{0xff, 0xd8, 2, 0xff, 0xd9}})

	token, err := server.auth.RotateBearer()
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v3/entrypoints/main/snapshot/ring.jpg", nil)
	request.Header.Set("Authorization", "Bearer "+token)

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/jpeg" || response.Body.String() != string([]byte{0xff, 0xd8, 2, 0xff, 0xd9}) {
		t.Fatalf("ring snapshot response = %d, headers = %#v, body = %v", response.Code, response.Header(), response.Body.Bytes())
	}
}

func TestServer_StateCarriesSnapshotTimes(t *testing.T) {
	server, store := newTestServer(t)
	ringAt := time.Date(2026, 10, 22, 9, 30, 0, 123456789, time.UTC)
	server.SetSnapshot(snapshotRecorder{ringAt: ringAt})

	token, err := auth.NewStore(store).RotateBearer()
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v3/state", nil)
	request.Header.Set("Authorization", "Bearer "+token)

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	var body struct {
		State struct {
			Entrypoints []map[string]any `json:"entrypoints"`
		} `json:"state"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.State.Entrypoints) != 1 {
		t.Fatalf("state response = %s", response.Body.String())
	}

	entrypoint := body.State.Entrypoints[0]
	if got := entrypoint["ring_snapshot_updated_at"]; got != "2026-10-22T09:30:00.123Z" {
		t.Fatalf("ring_snapshot_updated_at = %#v, want 2026-10-22T09:30:00.123Z", got)
	}

	if _, present := entrypoint["snapshot_updated_at"]; present {
		t.Fatalf("snapshot_updated_at = %#v, want it omitted when there is no snapshot", entrypoint["snapshot_updated_at"])
	}
}
```

In `TestServer_WebRTCContract`, after the existing `closeMessage` assertion, append:

```go
	offer = write(`{"type":"offer","id":"session-2","payload":{"session_id":"session-2","entrypoint_id":"main","origin":"native_camera","offer_sdp":"offer-sdp"}}`)
	if offer["type"] != "answer" {
		t.Fatalf("second offer response = %#v", offer)
	}

	leaveMessage := write(`{"type":"close","id":"session-2","payload":{"session_id":"session-2","reason":"answered_elsewhere","leave":true}}`)
	if leaveMessage["type"] != "ack" || control.left != "session-2" || control.closed != "session-1" {
		t.Fatalf("leave response = %#v, call = %#v", leaveMessage, control)
	}
```

Make sure `"time"` is imported in `api_test.go`.

- [ ] **Step 2: Confirm the tests compile but cannot pass yet**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/api/`
Expected: no output — the recorders only gain methods. The tests would fail if run (no `ring.jpg` route: 404; no `ring_snapshot_updated_at` field; `close` ignores `leave`), but `internal/api` tests do not run on this host (see Global Constraints). Step 3 adds the behaviour.

- [ ] **Step 3: Implement.**

`internal/api/interfaces.go`:

```go
type WebRTCControl interface {
	Offer(ctx context.Context, sessionID, entrypointID, offerSDP string, iceServers []media.ICEServer, onLocalCandidate func(*media.ICECandidate)) (string, error)
	AddICECandidate(sessionID string, candidate media.ICECandidate) error
	Close(sessionID string) error
	Leave(sessionID string) error
}

type SnapshotControl interface {
	Latest(entrypointID string) ([]byte, error)
	LatestRing(entrypointID string) ([]byte, error)
	UpdatedAt(entrypointID string) (latest, ring time.Time)
}
```

(add `"time"` to its imports).

`internal/api/handlers.go` — replace `snapshotLatest` with:

```go
func (s *Server) snapshotLatest(w http.ResponseWriter, r *http.Request) {
	s.serveSnapshot(w, r, SnapshotControl.Latest)
}

// snapshotRing serves the picture taken at the last doorbell ring.
func (s *Server) snapshotRing(w http.ResponseWriter, r *http.Request) {
	s.serveSnapshot(w, r, SnapshotControl.LatestRing)
}

func (s *Server) serveSnapshot(w http.ResponseWriter, r *http.Request, read func(SnapshotControl, string) ([]byte, error)) {
	if s.snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "snapshot is unavailable")
		return
	}

	image, err := read(s.snapshot, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, media.ErrSnapshotNotFound) {
			writeError(w, http.StatusNotFound, "snapshot_not_found", "snapshot is not available yet")
			return
		}

		writeCommandError(w, err)

		return
	}

	writeJPEG(w, image)
}
```

`internal/api/api.go` — after the `latest.jpg` route add:

```go
	s.handleProtected(mux, "GET", "/api/v3/entrypoints/{id}/snapshot/ring.jpg", s.snapshotRing)
```

and in `currentPayload` replace the append with:

```go
			dto.Entrypoints = append(dto.Entrypoints, s.withSnapshotTimes(entrypointDTO(entrypoint, s.entrypoints != nil)))
```

and add the method:

```go
// withSnapshotTimes adds when the entrypoint's pictures were taken. Home
// Assistant's doorbell image entity changes state on ring_snapshot_updated_at.
// See docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md.
func (s *Server) withSnapshotTimes(dto EntrypointDTO) EntrypointDTO {
	if s.snapshot == nil {
		return dto
	}

	latest, ring := s.snapshot.UpdatedAt(dto.ID)
	dto.SnapshotUpdatedAt = optionalTime(latest)
	dto.RingSnapshotUpdatedAt = optionalTime(ring)

	return dto
}

// optionalTime drops sub-millisecond digits, which Python's parser before 3.11
// rejects and nobody needs, and turns the zero time into an omitted field.
func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}

	value = value.UTC().Truncate(time.Millisecond)

	return &value
}
```

(add `"time"` to `api.go` imports if missing).

`internal/api/dto.go` — add to `EntrypointDTO`, and `"time"` to the imports:

```go
	SnapshotUpdatedAt     *time.Time      `json:"snapshot_updated_at,omitempty"`
	RingSnapshotUpdatedAt *time.Time      `json:"ring_snapshot_updated_at,omitempty"`
```

`internal/api/websocket.go` — add to `webrtcClosePayload`:

```go
	// Leave closes the session without ending the call: the card steps aside
	// because the call was answered on another device.
	Leave bool `json:"leave"`
```

and in `case "close":` replace `_ = s.webrtc.Close(sessionID)` with:

```go
		if payload.Leave {
			_ = s.webrtc.Leave(sessionID)
		} else {
			_ = s.webrtc.Close(sessionID)
		}
```

- [ ] **Step 4: Type-check and build**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/api/ ./internal/app/ && GOOS=linux GOARCH=arm GOARM=7 go build ./...`
Expected: no output. (`*media.SnapshotManager` and `*media.WebRTCService` satisfy the widened interfaces through Tasks 4 and 5.)

- [ ] **Step 5: Commit**

```bash
git add internal/api/interfaces.go internal/api/handlers.go internal/api/api.go internal/api/dto.go internal/api/websocket.go internal/api/api_test.go
git commit -m "feat(api): serve the doorbell picture, its time, and a leaving close

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Integration — timestamps, ring picture client, leaving close

All paths in this task are relative to `D:/Progetti/BTicinoGO-Integration`.

**Files:**
- Modify: `custom_components/bticino_companion/models.py` (`Entrypoint`), `const.py`, `api.py`, `camera.py`, `websocket_api.py`
- Test: `tests/test_models_protocol.py`, `tests/test_entities.py`

**Interfaces:**
- Consumes: Task 7's wire contract.
- Produces:
  - `Entrypoint.snapshot_updated_at: datetime | None`, `Entrypoint.ring_snapshot_updated_at: datetime | None` (timezone-aware).
  - `CompanionApiClient.async_entrypoint_snapshot_ring(entrypoint_id: str) -> bytes | None`
  - `CompanionApiClient.async_webrtc_close(*, session_id: str, leave: bool = False)`
  - `CompanionEntrypointCamera.async_close_card_webrtc_session(session_id: str, leave: bool = False)`
  - Card command `bticino_companion/card_webrtc_close` accepts optional `leave: bool`.

- [ ] **Step 1: Branch**

```bash
cd /d/Progetti/BTicinoGO-Integration && git fetch origin && git switch -c feat/shared-ringing-stream origin/v3
```

- [ ] **Step 2: Write the failing model test** — append to `class ModelsTest` in `tests/test_models_protocol.py`:

```python
    def test_entrypoint_parses_snapshot_times(self) -> None:
        entrypoint = Entrypoint.from_dict(
            {
                "id": "main",
                "snapshot_updated_at": "2026-10-22T09:30:00.123Z",
                "ring_snapshot_updated_at": "not a time",
            }
        )

        self.assertEqual(entrypoint.snapshot_updated_at.isoformat(), "2026-10-22T09:30:00.123000+00:00")
        self.assertIsNone(entrypoint.ring_snapshot_updated_at)
        self.assertIsNone(Entrypoint.from_dict({"id": "main"}).snapshot_updated_at)
```

- [ ] **Step 3: Run it to verify it fails**

Run: `python -m pytest tests/test_models_protocol.py -q -k snapshot_times`
Expected: FAIL with `AttributeError: 'Entrypoint' object has no attribute 'snapshot_updated_at'`.

- [ ] **Step 4: Implement the model** — in `models.py`, add `from datetime import datetime, timezone` to the imports, extend `Entrypoint`:

```python
@dataclass(frozen=True, slots=True)
class Entrypoint:
    """A configured intercom entrypoint."""

    id: str
    label: str
    devaddr: str
    capabilities: Capability
    availability: Availability
    snapshot_updated_at: datetime | None = None
    ring_snapshot_updated_at: datetime | None = None

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> "Entrypoint":
        return cls(
            id=str(payload.get("id", "")).strip(),
            label=str(payload.get("label", "")).strip(),
            devaddr=str(payload.get("devaddr", "")).strip(),
            capabilities=Capability.from_dict(mapping_at(payload, "capabilities")),
            availability=Availability.from_dict(mapping_at(payload, "availability")),
            snapshot_updated_at=_optional_datetime(payload.get("snapshot_updated_at")),
            ring_snapshot_updated_at=_optional_datetime(payload.get("ring_snapshot_updated_at")),
        )
```

and add next to `_optional_string`:

```python
def _optional_datetime(value: Any) -> datetime | None:
    """Parse an RFC 3339 time from the Companion, or return None."""
    if not isinstance(value, str) or not value.strip():
        return None
    text = value.strip()
    if text.endswith("Z"):
        text = f"{text[:-1]}+00:00"
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)
```

- [ ] **Step 5: Run the model tests**

Run: `python -m pytest tests/test_models_protocol.py -q`
Expected: all pass.

- [ ] **Step 6: Client, camera and command** —

`const.py`, after `API_PATH_SNAPSHOT_LATEST`:

```python
API_PATH_SNAPSHOT_RING = "/api/v3/entrypoints/{entrypoint_id}/snapshot/ring.jpg"
```

`api.py` — import `API_PATH_SNAPSHOT_RING` next to `API_PATH_SNAPSHOT_LATEST`, replace `async_entrypoint_snapshot_latest` with:

```python
    async def async_entrypoint_snapshot_latest(self, entrypoint_id: str) -> bytes | None:
        """Return the last passive snapshot without requesting a new capture."""
        return await self._async_snapshot(API_PATH_SNAPSHOT_LATEST, entrypoint_id)

    async def async_entrypoint_snapshot_ring(self, entrypoint_id: str) -> bytes | None:
        """Return the picture taken at the last doorbell ring, if any."""
        return await self._async_snapshot(API_PATH_SNAPSHOT_RING, entrypoint_id)

    async def _async_snapshot(self, path: str, entrypoint_id: str) -> bytes | None:
        if not self._access_token:
            raise CompanionAuthError("an access token is required")
        try:
            async with self._session.get(
                f"{self._base_url}{path.format(entrypoint_id=quote(entrypoint_id, safe=''))}",
                headers={"Accept": "image/jpeg", "Authorization": f"Bearer {self._access_token}"},
            ) as response:
                if response.status == 404:
                    return None
                if response.status >= 400:
                    self._raise_for_status(response, await self._async_json(response))
                image = await response.read()
                return image or None
        except ClientError as err:
            raise CompanionApiError("unable to contact Companion") from err
```

and replace `async_webrtc_close` with:

```python
    async def async_webrtc_close(self, *, session_id: str, leave: bool = False) -> dict[str, Any]:
        """Close a Companion WebRTC session and its signaling socket.

        leave closes it without ending the call: the card steps aside because the
        call was answered on another device.
        """
        session = self._webrtc_sessions.get(session_id)
        if session is None:
            return {"session_id": session_id}
        payload: dict[str, Any] = {"session_id": session_id, "reason": "ha_session_closed"}
        if leave:
            payload.update(reason="answered_elsewhere", leave=True)
        try:
            return await self._async_webrtc_message(session, "close", session_id, payload)
        except ClientError as err:
            raise CompanionApiError("unable to close Companion WebRTC session") from err
        finally:
            await self._async_webrtc_discard(session_id)
```

`camera.py` — replace `async_close_card_webrtc_session` and `_async_close_webrtc_session` with:

```python
    async def async_close_card_webrtc_session(self, session_id: str, leave: bool = False) -> None:
        """Close a bundled-card session only when it belongs to this camera."""
        if session_id not in self._active_webrtc_sessions:
            return
        self._active_webrtc_sessions.remove(session_id)
        await self._async_close_webrtc_session(session_id, leave=leave)
```

```python
    async def _async_close_webrtc_session(self, session_id: str, *, leave: bool = False) -> None:
        # Passed only when set, so the ordinary close stays the call it always was.
        kwargs = {"leave": True} if leave else {}
        try:
            await self._client.async_webrtc_close(session_id=session_id, **kwargs)
        except CompanionApiError as err:
            _LOGGER.debug("Unable to close Companion WebRTC session %s: %s", session_id, err)
```

`websocket_api.py` — the close command schema and handler:

```python
@callback
@websocket_api.websocket_command(
    {
        vol.Required("type"): "bticino_companion/card_webrtc_close",
        vol.Required("camera_entity_id"): str,
        vol.Required("session_id"): str,
        vol.Optional("leave", default=False): bool,
    }
)
@websocket_api.async_response
async def _handle_close(hass: HomeAssistant, connection, msg: dict[str, Any]) -> None:
    """Close only the session owned by the configured camera entity."""
    try:
        await _camera_for_message(hass, msg).async_close_card_webrtc_session(
            str(msg["session_id"]).strip(), leave=msg["leave"]
        )
    except (CompanionApiError, HomeAssistantError, ValueError) as err:
        connection.send_error(msg["id"], "webrtc_close_failed", str(err))
        return
    connection.send_result(msg["id"])
```

- [ ] **Step 7: Add the camera leave test** — append to `TypedControlTest` in `tests/test_entities.py`:

```python
    async def test_camera_card_session_can_leave_without_ending_the_call(self) -> None:
        entry = _MockEntry()
        stream = Entrypoint.from_dict(
            {"id": "main", "label": "Main", "capabilities": {"stream": True}, "availability": {"stream": True}}
        )
        camera = CompanionEntrypointCamera(
            entry, _coordinator(_state(entrypoints=(stream,))), entry.runtime_data.client, "main", "Main"
        )

        await camera.async_handle_card_webrtc_offer("offer-sdp", "session-1")
        await camera.async_close_card_webrtc_session("session-1", leave=True)

        entry.runtime_data.client.async_webrtc_close.assert_awaited_once_with(session_id="session-1", leave=True)
```

- [ ] **Step 8: Run what runs locally and compile the rest**

Run: `python -m pytest tests -q && python -m compileall -q custom_components`
Expected: model/protocol tests pass; `test_entities.py` and others needing Home Assistant are skipped; compileall prints nothing. Report that the entity tests were skipped.

- [ ] **Step 9: Commit**

```bash
git add custom_components/bticino_companion/models.py custom_components/bticino_companion/const.py custom_components/bticino_companion/api.py custom_components/bticino_companion/camera.py custom_components/bticino_companion/websocket_api.py tests/test_models_protocol.py tests/test_entities.py
git commit -m "feat: read snapshot times, fetch the doorbell picture, close a card session by leaving

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: Integration — "Last doorbell ring" image entity

**Files:**
- Create: `custom_components/bticino_companion/image.py`
- Modify: `custom_components/bticino_companion/const.py` (`PLATFORMS`), `custom_components/bticino_companion/dynamic_entities.py`, `README.md`
- Test: `tests/test_entities.py`

**Interfaces:**
- Consumes: Task 8's `Entrypoint.ring_snapshot_updated_at`, `async_entrypoint_snapshot_ring`.
- Produces: `CompanionRingSnapshotImage(hass, entry, coordinator, client, entrypoint_id, name)`, unique id `f"{entry.unique_id}_ring_snapshot_{entrypoint_id}"`, one per stream-capable entrypoint.

- [ ] **Step 1: Write the failing tests** — in `tests/test_entities.py`, add `from bticino_companion.image import CompanionRingSnapshotImage` and `from datetime import datetime, timezone` and `from unittest.mock import patch` to the imports (the HA import guard stays), add `self.runtime_data.client.async_entrypoint_snapshot_ring = AsyncMock(return_value=b"jpeg")` to `_MockEntry.__init__`, and append:

```python
class RingSnapshotImageTest(unittest.IsolatedAsyncioTestCase):
    def _image(self, entry, entrypoint) -> CompanionRingSnapshotImage:
        with patch("homeassistant.components.image.get_async_client"):
            return CompanionRingSnapshotImage(
                MagicMock(), entry, _coordinator(_state(entrypoints=(entrypoint,))), entry.runtime_data.client, "main", "Main"
            )

    async def test_state_is_the_time_of_the_last_ring_picture(self) -> None:
        entry = _MockEntry()
        rang = datetime(2026, 10, 22, 9, 30, tzinfo=timezone.utc)
        entrypoint = Entrypoint.from_dict({"id": "main", "capabilities": {"stream": True}})
        image = self._image(entry, entrypoint)

        self.assertIsNone(image.image_last_updated)

        image.coordinator.data = _state(
            entrypoints=(Entrypoint.from_dict(
                {"id": "main", "capabilities": {"stream": True}, "ring_snapshot_updated_at": rang.isoformat()}
            ),)
        )

        self.assertEqual(image.image_last_updated, rang)
        self.assertEqual(image.unique_id, "device-123_ring_snapshot_main")

    async def test_fetches_the_picture_once_per_ring(self) -> None:
        entry = _MockEntry()
        entrypoint = Entrypoint.from_dict(
            {"id": "main", "capabilities": {"stream": True}, "ring_snapshot_updated_at": "2026-10-22T09:30:00Z"}
        )
        image = self._image(entry, entrypoint)

        self.assertEqual(await image.async_image(), b"jpeg")
        self.assertEqual(await image.async_image(), b"jpeg")
        entry.runtime_data.client.async_entrypoint_snapshot_ring.assert_awaited_once_with("main")

    async def test_no_picture_before_the_first_ring(self) -> None:
        entry = _MockEntry()
        image = self._image(entry, Entrypoint.from_dict({"id": "main", "capabilities": {"stream": True}}))

        self.assertIsNone(await image.async_image())
        entry.runtime_data.client.async_entrypoint_snapshot_ring.assert_not_awaited()
```

and in `DynamicEntityManagerTest` append:

```python
    async def test_reconciles_one_ring_image_per_stream_capable_entrypoint(self) -> None:
        entry = _MockEntry()
        stream = Entrypoint.from_dict({"id": "main", "capabilities": {"stream": True}})
        entry.runtime_data.coordinator = _coordinator(_state(entrypoints=(stream,)))
        manager = DynamicEntityManager(MagicMock(), entry, entry.runtime_data.coordinator, entry.runtime_data.client)
        platform = _MockPlatform()

        with patch("homeassistant.components.image.get_async_client"):
            await manager.async_register_platform("image", platform)

        self.assertEqual([entity.unique_id for entity in platform.added], ["device-123_ring_snapshot_main"])
```

- [ ] **Step 2: Implement** — create `custom_components/bticino_companion/image.py`:

```python
"""Doorbell picture image entities."""

from __future__ import annotations

from datetime import datetime
import logging

from homeassistant.components.image import ImageEntity
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddEntitiesCallback, async_get_current_platform
from homeassistant.helpers.update_coordinator import CoordinatorEntity

from . import IntegrationRuntime
from .api import CompanionApiError
from .coordinator import CompanionCoordinator
from .device_info import device_info
from .entity import CompanionAvailabilityMixin
from .models import Entrypoint

_LOGGER = logging.getLogger(__name__)


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities: AddEntitiesCallback
) -> None:
    """Register dynamic doorbell picture entities."""
    del hass, async_add_entities
    runtime: IntegrationRuntime = entry.runtime_data
    await runtime.dynamic_entities.async_register_platform("image", async_get_current_platform())


class CompanionRingSnapshotImage(CompanionAvailabilityMixin, CoordinatorEntity[CompanionCoordinator], ImageEntity):
    """The picture taken at the last doorbell ring of one entrypoint.

    Its state is the time of that picture, so it changes only on a new ring.
    Notification automations trigger on it rather than on Ringing, whose picture
    is not taken yet when the ring starts. A live view never changes it.
    """

    _attr_has_entity_name = True
    _attr_icon = "mdi:doorbell-video"
    _attr_content_type = "image/jpeg"

    def __init__(
        self,
        hass: HomeAssistant,
        entry: ConfigEntry,
        coordinator: CompanionCoordinator,
        client,
        entrypoint_id: str,
        name: str,
    ) -> None:
        CoordinatorEntity.__init__(self, coordinator)
        ImageEntity.__init__(self, hass)
        self._entry = entry
        # ImageEntity keeps its own HTTP client in self._client.
        self._companion = client
        self._entrypoint_id = entrypoint_id
        self._attr_name = f"{name} last doorbell ring"
        self._attr_unique_id = f"{entry.unique_id}_ring_snapshot_{entrypoint_id}"
        self._cached: bytes | None = None
        self._cached_at: datetime | None = None

    @property
    def device_info(self):
        return device_info(self._entry, self.coordinator.data)

    def _entrypoint(self) -> Entrypoint | None:
        state = self.coordinator.data
        if not state:
            return None
        return next((item for item in state.entrypoints if item.id == self._entrypoint_id), None)

    @property
    def available(self) -> bool:
        entrypoint = self._entrypoint()
        return bool(super().available and entrypoint and entrypoint.capabilities.stream)

    @property
    def image_last_updated(self) -> datetime | None:
        """The time of the last doorbell picture: the entity's state."""
        entrypoint = self._entrypoint()
        return entrypoint.ring_snapshot_updated_at if entrypoint else None

    async def async_image(self) -> bytes | None:
        """Return the doorbell picture, fetched once per new ring."""
        updated = self.image_last_updated
        if updated is None:
            return None
        if self._cached is not None and self._cached_at == updated:
            return self._cached
        try:
            image = await self._companion.async_entrypoint_snapshot_ring(self._entrypoint_id)
        except CompanionApiError as err:
            _LOGGER.debug("Unable to fetch the doorbell picture for %s: %s", self._entrypoint_id, err)
            return None
        self._cached, self._cached_at = image, updated
        return image
```

`const.py` — add `"image",` to `PLATFORMS` after `"camera",`.

`dynamic_entities.py` — add `from .image import CompanionRingSnapshotImage` to the local imports in `_desired_entities`, and inside `if entrypoint.capabilities.stream:`, after the camera's `desired.append(...)`, add:

```python
                desired.append(
                    _DesiredEntity(
                        "image",
                        f"{self._entry.unique_id}_ring_snapshot_{entrypoint.id}",
                        lambda entrypoint=entrypoint: CompanionRingSnapshotImage(
                            self._hass,
                            self._entry,
                            self._coordinator,
                            self._client,
                            entrypoint.id,
                            entrypoint.label or entrypoint.id,
                        ),
                    )
                )
```

- [ ] **Step 3: README** — in `README.md`, add a section after the intercom card section:

```markdown
## Doorbell notifications with the picture

Every stream-capable entrypoint gets an image entity, **"<entrypoint> last doorbell ring"**. The
Companion takes its picture about 2–3 seconds after each SIP ring, and the entity's state is the
time of that picture: it changes only on a new ring, never on a live view. Trigger notifications on
it rather than on the Ringing sensor, which turns on before the picture exists.

Pick the entity id from the entity list (it depends on your installation's language), then:

```yaml
automation:
  - alias: Doorbell notification
    triggers:
      - trigger: state
        entity_id: image.REPLACE_WITH_YOUR_ENTITY
    conditions:
      - condition: template
        value_template: "{{ trigger.to_state.state not in ['unknown', 'unavailable'] }}"
    actions:
      - action: notify.mobile_app_REPLACE_WITH_YOUR_PHONE
        data:
          message: Someone is at the door
          data:
            image: /api/image_proxy/image.REPLACE_WITH_YOUR_ENTITY
```

If the Companion cannot open the stream during a ring, no picture is taken and the entity does not
change, so no notification is sent for that ring.
```

- [ ] **Step 4: Compile and run what runs locally**

Run: `python -m compileall -q custom_components && python -m pytest tests -q`
Expected: compileall prints nothing; HA-dependent tests skipped. Report it.

- [ ] **Step 5: Commit**

```bash
git add custom_components/bticino_companion/image.py custom_components/bticino_companion/const.py custom_components/bticino_companion/dynamic_entities.py README.md tests/test_entities.py
git commit -m "feat: add a 'last doorbell ring' image entity for notifications

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Card — answered elsewhere closes the view

**Files:**
- Modify: `custom_components/bticino_companion/www/bticino-go-intercom-card.js`

**Interfaces:**
- Consumes: Task 8's `card_webrtc_close` with `leave`.

No automated tests exist for the card; it is verified manually in Task 11.

- [ ] **Step 1: State** — in the constructor, after `this._answering = false;`, add:

```js
    // True once this card has asked to answer the current call, so a later
    // "active" is recognised as ours and not as an answer on another device.
    this._answeredHere = false;
    // True after this card closed its view because another device answered;
    // cleared when that call ends.
    this._leftForElsewhere = false;
```

- [ ] **Step 2: Leave on an answer elsewhere** — in `set hass`, replace the block inside `if (this._pc || this._connecting) {` (the comment and the `if (this._preview && !this._answering ...)` statement) with:

```js
      // The call was answered through the companion by another device: this
      // card steps aside. Closing with leave never ends the call, which the
      // answering device now carries. See
      // docs/superpowers/specs/2026-10-22-shared-ringing-stream-design.md in the
      // Companion repository.
      if (!this._answering && !this._answeredHere && this._callState() === "active") {
        this._leftForElsewhere = true;
        this._cleanup({ leave: true });
        this._render();
        return;
      }
```

and, right after the opening `this._endPreviewIfRingOver();` line of `set hass`, add:

```js
    if (this._leftForElsewhere && this._callState() !== "active") this._leftForElsewhere = false;
```

- [ ] **Step 3: No preview or live view while another device talks** — at the start of the preview branch condition in `set hass`, change

```js
    } else if (!this._previewTried && this.isConnected) {
```

to

```js
    } else if (!this._previewTried && !this._leftForElsewhere && this.isConnected) {
```

- [ ] **Step 4: Record our own answer** — in `_answer()`, add `this._answeredHere = true;` as the first line after `if (this._preview && (this._pc || this._connecting)) return this._answerPreview();`. In `_answerPreview()`, add `this._answeredHere = true;` right after `this._answering = true;`.

- [ ] **Step 5: Close with leave** — replace `_cleanup()`'s signature and its close message:

```js
  _cleanup({ leave = false } = {}) {
```

```js
    if (sessionId && this._hass) {
      this._send("bticino_companion/card_webrtc_close", { session_id: sessionId, ...(leave ? { leave: true } : {}) }).catch(() => {});
    }
```

and add `this._answeredHere = false;` next to `this._preview = false;` in `_cleanup`.

- [ ] **Step 6: Show the notice** — in `_render()`, make the status and the live-view button reflect it. Replace the first line of the `status` expression:

```js
    const status = this._elsewhereUntil
      ? ["Answered elsewhere", "idle"]
```

with

```js
    const status = this._leftForElsewhere && inCall
      ? ["In a call on another device", "active"]
      : this._elsewhereUntil
      ? ["Answered elsewhere", "idle"]
```

and in the `.call` button's class expression add `|| (this._leftForElsewhere && inCall)` to the hide condition:

```js
        <button class="call ${(active && !this._preview) || (ringing && !canAnswer) || (this._leftForElsewhere && inCall) ? "hidden" : ""}" title="${canAnswer ? "Answer" : "Start live view"}"><ha-icon icon="mdi:phone"></ha-icon></button>
```

(`inCall` is declared before `status` already; keep the hang-up button as it is — the comment above `inCall` explains why it must stay reachable.)

- [ ] **Step 7: Syntax check**

Run: `node --check custom_components/bticino_companion/www/bticino-go-intercom-card.js`
Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add custom_components/bticino_companion/www/bticino-go-intercom-card.js
git commit -m "feat(card): step aside when the call is answered on another device

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: Build, install, verify on the intercom

**Files:** none changed except the local build output (`dist/companion` stays unstaged).

- [ ] **Step 1: Full Linux build and vet**

Run (in `D:/Progetti/BTicinoGO`): `GOOS=linux GOARCH=arm GOARM=7 go vet ./... && go test ./internal/core/ ./internal/signaling/ ./internal/openwebnet/`
Expected: vet prints nothing; the three native packages `ok`.

- [ ] **Step 2: Build the binary** — follow `docs/build-and-install.md` exactly for the ARM build and the install over SSH. Do not run the install: hand the commands to the user, who runs them against their intercom (its address is not known to this session).

- [ ] **Step 3: Hand the user the manual checks**, to be done after installing the companion build and updating the integration (restart Home Assistant, hard-refresh the browser so the card reloads):

1. Card open on two devices, ring: both show the video while ringing.
2. Answer on one: the other closes its view and shows "In a call on another device"; audio and microphone work on the answering one.
3. Hang up: no "Ringing" afterwards (`binary_sensor.*_ringing` stays off; check its history).
4. With every card closed, ring: within about 3 s the "last doorbell ring" image entity changes and shows this visitor; the example automation from the README sends the notification with that picture.
5. A live view with no ring does not change the image entity.
6. In `/tmp/companion.log`: `stream consumer joined` for the second device, no `superseded by successor session`, and `ring holder` warnings only if the stream really failed.

- [ ] **Step 4: Report** — what was built, which tests ran (core, signaling, openwebnet, integration models) and which were only type-checked (`internal/media`, `internal/app`, `internal/api`, HA entity tests), and the manual checks still pending on the device.

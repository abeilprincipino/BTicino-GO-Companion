# Inbound Call Stream Ownership Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an answered inbound doorbell call obtain the media lease, so the Home Assistant card can show video instead of failing with `media: intercom stream is active externally`.

**Architecture:** `signaling.Manager` exposes a read-only predicate saying whether the active dialog is an answered inbound call. `media.StreamCoordinator` accepts that predicate as an injected function and consults it in `reserve()` before refusing an externally owned stream. `internal/app/run.go` wires the two. No new media source, no change to the lease model, no change to teardown.

**Tech Stack:** Go, standard library `testing`. Existing test helpers: `testManagedSourceFactory`, `beginControlAttempt`, `endControlAttempt` (media); `fakeIncomingDialog`, `fakeDialer`, `syncEventSink`, `testResolver`, `testDialogID` (signaling).

## Global Constraints

- `internal/media` must not import `internal/signaling`. The dependency is a plain injected `func() bool`.
- A `nil` probe must reproduce today's behaviour exactly: every externally owned stream is refused.
- The lease stays exclusive. `ErrStreamBusy` for `leaseID != 0` is unaffected by the probe.
- The probe must never be called while `c.mu` is held (deadlock ordering — see Task 2, Step 3).
- Spec: `docs/superpowers/specs/2026-08-02-inbound-call-stream-ownership-design.md`.

---

### Task 1: The answered-call predicate on the signaling manager

**Files:**
- Modify: `internal/signaling/manager.go` (add a method after `RemoteDialogEnded`, which ends at line 346)
- Test: `internal/signaling/manager_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func (m *Manager) HasAnsweredInboundCall() bool` — Task 3 passes this method value to the coordinator.

- [ ] **Step 1: Write the failing tests**

Append to `internal/signaling/manager_test.go`:

```go
func TestManager_HasAnsweredInboundCallTracksTheCallLifecycle(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))

	if manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = true with no call, want false")
	}

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = true while ringing, want false")
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = false after Answer, want true")
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatal(err)
	}

	if manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = true after Hangup, want false")
	}
}

func TestManager_HasAnsweredInboundCallIsFalseAfterRemoteBye(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	manager.RemoteDialogEnded()

	if manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = true after the peer ended the dialog, want false")
	}
}

func TestManager_HasAnsweredInboundCallIsFalseForOutgoingPreview(t *testing.T) {
	t.Parallel()

	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))
	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatal(err)
	}

	if manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = true for an outgoing preview, want false")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/signaling/ -run TestManager_HasAnsweredInboundCall -v`
Expected: FAIL to build, with `manager.HasAnsweredInboundCall undefined (type *Manager has no field or method HasAnsweredInboundCall)`.

- [ ] **Step 3: Write the implementation**

In `internal/signaling/manager.go`, insert after the closing brace of `RemoteDialogEnded` (line 346):

```go
// HasAnsweredInboundCall reports whether the active dialog is an inbound call
// the companion has answered.
//
// The media coordinator asks this before refusing a lease for a stream it sees
// as externally owned. The intercom starts its AV while the call is still
// ringing, so the stream is already marked external by the time the user
// answers; without this the answered call could never obtain a lease and the
// card would never show video. See
// docs/superpowers/specs/2026-08-02-inbound-call-stream-ownership-design.md.
func (m *Manager) HasAnsweredInboundCall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.active != nil && m.activeIncoming
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/signaling/ -run TestManager_HasAnsweredInboundCall -v`
Expected: PASS, three tests.

Then run the whole package to prove nothing regressed:

Run: `go test ./internal/signaling/`
Expected: `ok  	bticino-go-companion/internal/signaling`

- [ ] **Step 5: Commit**

```bash
git add internal/signaling/manager.go internal/signaling/manager_test.go
git commit -m "feat(signaling): expose whether an inbound call is answered"
```

---

### Task 2: The coordinator probe and the relaxed guard

**Files:**
- Modify: `internal/media/stream_coordinator.go` (struct at lines 96-111, new setter after `SetStateObserver` which ends at line 123, `reserve` at lines 177-203)
- Test: `internal/media/stream_coordinator_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 at compile time — the probe is a plain `func() bool`.
- Produces: `func (c *StreamCoordinator) SetAnsweredCallProbe(probe func() bool)` — Task 3 calls it.

- [ ] **Step 1: Write the failing tests**

Append to `internal/media/stream_coordinator_test.go`:

```go
func TestStreamCoordinatorAdoptsExternalStreamForAnsweredCall(t *testing.T) {
	c := NewStreamCoordinator(nil, testManagedSourceFactory())
	c.SetAnsweredCallProbe(func() bool { return true })

	// The intercom starts its own AV while the call rings, which is what marks
	// the stream external before anyone asks for a lease.
	c.ObserveControlTrack(true)

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatalf("Acquire() error = %v, want nil", err)
	}

	if owner := c.Snapshot().Owner; owner != StreamOwnerCompanion {
		t.Fatalf("owner = %q, want %q", owner, StreamOwnerCompanion)
	}

	if !c.Release(lease) {
		t.Fatal("release source lease")
	}
}

func TestStreamCoordinatorRejectsExternalStreamWithoutAnsweredCall(t *testing.T) {
	c := NewStreamCoordinator(nil, testManagedSourceFactory())
	c.SetAnsweredCallProbe(func() bool { return false })
	c.ObserveControlTrack(true)

	_, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if !errors.Is(err, ErrExternalStream) {
		t.Fatalf("Acquire() error = %v, want ErrExternalStream", err)
	}
}

func TestStreamCoordinatorAnsweredCallDoesNotOverrideABusyLease(t *testing.T) {
	c := NewStreamCoordinator(nil, testManagedSourceFactory())
	c.SetAnsweredCallProbe(func() bool { return true })

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if !errors.Is(err, ErrStreamBusy) {
		t.Fatalf("second Acquire() error = %v, want ErrStreamBusy — the lease stays exclusive", err)
	}

	if !c.Release(lease) {
		t.Fatal("release source lease")
	}
}
```

`TestStreamCoordinatorRejectsExternalStream`, already in this file at line 38, is the nil-probe case and must keep passing untouched.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/media/ -run TestStreamCoordinator -v`
Expected: FAIL to build, with `c.SetAnsweredCallProbe undefined (type *StreamCoordinator has no field or method SetAnsweredCallProbe)`.

- [ ] **Step 3: Add the field, the setter and the lock-free reader**

In `internal/media/stream_coordinator.go`, add a field to the `StreamCoordinator` struct, after `observer StreamStateObserver` (line 109):

```go
	// answeredCallProbe reports whether a stream seen as external belongs to an
	// inbound call the companion has answered. Nil until the application wires
	// signaling in, and a nil probe keeps the original behaviour of refusing
	// every externally owned stream.
	answeredCallProbe func() bool
```

Add the setter and the reader after `SetStateObserver` (which ends at line 123):

```go
// SetAnsweredCallProbe supplies the predicate that tells the coordinator whether
// a stream it sees as external belongs to a call the companion has answered.
// Such a stream is the companion's to take: the intercom starts its AV while
// ringing, long before the user answers, so without this an answered call could
// never reach a lease.
func (c *StreamCoordinator) SetAnsweredCallProbe(probe func() bool) {
	c.mu.Lock()
	c.answeredCallProbe = probe
	c.mu.Unlock()
}

// answeredCall runs the probe with c.mu released.
//
// The probe locks the signaling manager. Calling it under c.mu would order c.mu
// before that lock, the reverse of every path that starts in signaling and ends
// in a read of the coordinator snapshot, so the two orders together would
// deadlock. The verdict is therefore taken a moment before the critical section
// that uses it; a call cannot end in that window without the ordinary teardown
// path observing it.
func (c *StreamCoordinator) answeredCall() bool {
	c.mu.Lock()
	probe := c.answeredCallProbe
	c.mu.Unlock()

	return probe != nil && probe()
}
```

- [ ] **Step 4: Relax the guard in `reserve`**

Replace lines 177-184 of `internal/media/stream_coordinator.go`, which today read:

```go
func (c *StreamCoordinator) reserve(ctx context.Context, entrypoint config.Entrypoint) (*StreamLease, error) {
	c.mu.Lock()
	if c.snapshot.Owner == StreamOwnerExternal {
		c.mu.Unlock()
		c.logger.DebugContext(ctx, "stream lease rejected", "entrypoint_id", entrypoint.ID, "reason", "external_stream_active")

		return nil, ErrExternalStream
	}
```

with:

```go
func (c *StreamCoordinator) reserve(ctx context.Context, entrypoint config.Entrypoint) (*StreamLease, error) {
	answered := c.answeredCall()

	c.mu.Lock()
	if c.snapshot.Owner == StreamOwnerExternal {
		if !answered {
			c.mu.Unlock()
			c.logger.DebugContext(ctx, "stream lease rejected", "entrypoint_id", entrypoint.ID, "reason", "external_stream_active")

			return nil, ErrExternalStream
		}

		c.logger.InfoContext(ctx, "external stream adopted for answered call", "entrypoint_id", entrypoint.ID)
	}
```

Leave the rest of `reserve` unchanged: the `leaseID != 0 || factory == nil` branch and everything below it stay exactly as they are.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/media/ -run TestStreamCoordinator -v`
Expected: PASS, including the pre-existing `TestStreamCoordinatorRejectsExternalStream`.

Run the package and the race detector, because the probe adds a cross-goroutine read:

Run: `go test -race ./internal/media/`
Expected: `ok  	bticino-go-companion/internal/media`

- [ ] **Step 6: Commit**

```bash
git add internal/media/stream_coordinator.go internal/media/stream_coordinator_test.go
git commit -m "fix(media): let an answered call take the external stream"
```

---

### Task 3: Wire the probe in the application

**Files:**
- Modify: `internal/app/run.go` (after the `NewRTSPServer` error check, which ends at line 388)

**Interfaces:**
- Consumes: `Manager.HasAnsweredInboundCall` from Task 1, `StreamCoordinator.SetAnsweredCallProbe` from Task 2.
- Produces: nothing further.

This task has no unit test of its own. It is a three-line wiring change whose failure mode is a build error or a dead probe, and it is verified by the build, the full suite, and the on-device check in Task 4.

- [ ] **Step 1: Add the wiring**

In `internal/app/run.go`, immediately after the `NewRTSPServer` error check that ends at line 388 and before `rtspServer.Start(ctx)`, insert:

```go
	// The coordinator refuses a lease for a stream it sees as externally owned.
	// On an inbound call the intercom starts its AV while ringing, so that flag
	// is set well before the user answers and the answered call would never get
	// a lease. The probe tells the coordinator when the external stream is in
	// fact the companion's own answered call. See
	// docs/superpowers/specs/2026-08-02-inbound-call-stream-ownership-design.md.
	rtspServer.Coordinator().SetAnsweredCallProbe(calls.HasAnsweredInboundCall)
```

`calls` is the `*signaling.Manager` built at line 373, so it is already in scope.

- [ ] **Step 2: Verify the build**

Run: `go build ./...`
Expected: no output.

- [ ] **Step 3: Run the whole suite**

Run: `go test ./...`
Expected: every package `ok` or `no test files`. No `FAIL`.

- [ ] **Step 4: Lint**

Run: `golangci-lint run`
Expected: no findings. If `golangci-lint` is not installed, run `go vet ./...` instead and expect no output.

- [ ] **Step 5: Commit**

```bash
git add internal/app/run.go
git commit -m "feat(app): wire the answered-call probe into the coordinator"
```

---

### Task 4: On-device verification

**Files:** none — this task changes no code. It is the gate that says the fix works on real hardware, and it is where the risk recorded in the spec gets tested.

**Interfaces:**
- Consumes: the deployed binary containing Tasks 1 to 3.

- [ ] **Step 1: Deploy the build to the device and restart the companion**

Follow `docs/build-and-install.md` for the cross-build and copy. Restart the service so the new binary runs:

```sh
/etc/init.d/companion restart
```

- [ ] **Step 2: Start the AV capture**

On the device:

```sh
tcpdump -i any -n -w /tmp/av.pcap 'udp port 5000 or udp port 5007' &
```

- [ ] **Step 3: Ring, answer from the card, and stay in the call for at least 30 seconds**

Press the doorbell at the outdoor station, answer from the Home Assistant card, and leave the call up. Thirty seconds is deliberate: it is long enough for the foreign-stop-frame risk in section 6 of the spec to show itself.

- [ ] **Step 4: Stop the capture and read it**

```sh
kill %1 2>/dev/null || killall tcpdump
tcpdump -r /tmp/av.pcap -n | head -20
```

Expected: RTP arriving at `127.0.0.1.5007` and `127.0.0.1.5000` from the moment of the answer. An empty capture means the lease was still refused — check the companion log for `stream lease rejected`.

- [ ] **Step 5: Check the companion log**

Expected, in order: `inbound sip invite received`, `external stream detected`, `call answered`, `external stream adopted for answered call`, and no `stream lease rejected reason=external_stream_active`. The card must show video.

- [ ] **Step 6: Record what happened to the known risk**

If the video drops on its own partway through the call, the foreign AV stop frame described in section 6 of the spec is real. Capture the evidence for the follow-up work:

```sh
grep -n "stream teardown started\|openwebnet stream stop" /tmp/companion.log | tail -20
```

A `stream teardown started` with reason `openwebnet stream stop` during a healthy call confirms it. Note the finding; do not fix it here — it is separate work with its own investigation into whether OpenWebNet AV frames can be attributed to a destination.

---

## Out of scope

The two flexisip provisioning defects recorded in section 8 of the spec are not part of this plan:

- `scripts/install.sh:280` adds the companion to the `alluser` group in `route_int.conf`, but flexisip reads `route.conf`.
- `scripts/install.sh:270` writes a static record of `<sip:127.0.0.1>`, which resolves to flexisip itself, and now also causes every inbound INVITE to be forked to the companion twice.

They need their own plan. Note that a device used for Task 4 must have had the `alluser` group fixed by hand first, or no inbound call reaches the companion at all.

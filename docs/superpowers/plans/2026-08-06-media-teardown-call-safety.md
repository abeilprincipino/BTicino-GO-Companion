# Media Teardown Call Safety Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop a media-layer teardown from ending the doorbell call, so that the internal unit's ringing leg timing out no longer hangs up the call the user is on.

**Architecture:** `Manager.StartStream` reports whether it created the SIP dialog; `SourceSession` remembers that as `ownsDialog` and refuses to send a BYE for a dialog it did not create, on both of the two independent paths that send one. A consumer releasing the lease still ends the call, declared explicitly through a typed teardown cause and an optional capability interface. Separately, a BYE on an answered inbound call now releases the media lease.

**Tech Stack:** Go, standard library `testing`, POSIX `sh` for the installer. Existing test helpers: `fakeSourceSIP`, `fakeSourceAV`, `fakeSourceReceiver`, `testManagedSourceFactory`, `beginControlAttempt`, `endControlAttempt` (media); `fakeIncomingDialog`, `fakeDialer`, `syncEventSink`, `testResolver`, `testDialogID`, `testInboundDialer`, `newRingingInboundHandler`, `testInviteRequest`, `waitRinging`, `serverSessionOf`, `waitDialogState`, `testAckRequest`, `testByeRequest`, `waitClosed` (signaling).

## Global Constraints

- Spec: `docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md`.
- `internal/media` production code must not import `internal/signaling`. The test package already does, and `var _ SourceSIP = (*signaling.Manager)(nil)` at `internal/media/source_session_test.go:17` is deliberate — keep it.
- An outbound preview must behave exactly as it does today. `ownsDialog == true` reproduces current behaviour on every path; every existing test that does not opt into adoption must pass unchanged.
- Safe by default: a teardown cause added in the future must not end a call unless it says so. `endsCall` is false in the zero value, and only `causeConsumerReleased` sets it true.
- This plan changes only *whether a teardown ends the SIP call*. It must not change *when* a teardown happens. Do not touch `ObserveControlStop`'s guards, `Reconcile`'s stall logic, or `ObserveControlTrack`.
- Task 5 is independent of Tasks 1-4 — it shares only the debugging session that found it. It may be executed and committed in any order relative to them.
- Out of scope, do not implement: confirming a stop frame against RTP flow; telling the intercom a call was answered elsewhere; removing bad static records from already-installed devices.

---

### Task 1: The SIP layer reports whether it created the dialog

**Files:**
- Modify: `internal/signaling/manager.go:438-464` (`StartStream`)
- Modify: `internal/media/source_session.go:15-24` (the `SourceSIP` interface), `:49-65` (the struct), `:88-93` (per-start reset), `:165-169` (the call site)
- Test: `internal/signaling/manager_test.go`, `internal/media/source_session_test.go:354-374` (the fakes)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func (m *Manager) StartStream(context.Context, string) (bool, error)` — reports `true` when it dialed, `false` when an answered inbound call meant no INVITE went out. `SourceSession.ownsDialog` holds the answer; Tasks 2 and 3 read it.

This task stores the fact and changes no behaviour. Tasks 2 and 3 act on it.

- [ ] **Step 1: Write the failing tests**

Append to `internal/signaling/manager_test.go`:

```go
func TestManager_StartStreamReportsThatItCreatedTheDialog(t *testing.T) {
	t.Parallel()

	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))

	created, err := manager.StartStream(context.Background(), "21")
	if err != nil {
		t.Fatal(err)
	}

	if !created {
		t.Fatal("StartStream() created = false for an outgoing preview, want true")
	}
}

func TestManager_StartStreamCreatesNoDialogDuringAnsweredInboundCall(t *testing.T) {
	t.Parallel()

	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), &fakeIncomingDialog{id: testDialogID}); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	created, err := manager.StartStream(context.Background(), "21")
	if err != nil {
		t.Fatal(err)
	}

	if created {
		t.Fatal("StartStream() created = true during an answered inbound call, want false — no INVITE goes out, so the dialog is the call's")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/signaling/ -run TestManager_StartStream -v`
Expected: FAIL to build, with `assignment mismatch: 2 variables but manager.StartStream returns 1 value`.

- [ ] **Step 3: Change `Manager.StartStream`**

Replace `internal/signaling/manager.go:438-464` in full:

```go
// StartStream dials the intercom and reports whether this call created the SIP
// dialog.
//
// It reports false when an answered inbound call is already up: the intercom is
// streaming for that call, no INVITE goes out, and the dialog belongs to the
// call rather than to the media session that asked. media.SourceSession needs
// that distinction to know whether a BYE of its own is ever appropriate — a
// teardown of its media must not end a call the user is on. See
// docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md.
func (m *Manager) StartStream(ctx context.Context, devAddr string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.incoming != nil {
		return false, ErrIncomingDialog
	}

	// The intercom is already streaming for the answered call; a second INVITE
	// would come back as 486 Busy Here.
	if m.activeIncoming {
		return false, nil
	}

	if m.active != nil {
		return false, ErrActiveDialog
	}

	dialog, err := m.dialer.StartStream(ctx, devAddr, BuildOffer(m.host, devAddr))
	if err != nil {
		return false, fmt.Errorf("outgoing stream: %w", err)
	}

	m.active = dialog

	return true, nil
}
```

- [ ] **Step 4: Update the `SourceSIP` interface**

Replace `internal/media/source_session.go:15-24`:

```go
type SourceSIP interface {
	// StartStream dials the intercom and reports whether it created the SIP
	// dialog. It reports false when an answered inbound call is already up: no
	// INVITE goes out and the dialog belongs to that call, so this session must
	// never send a BYE for it as a side effect of a media teardown.
	StartStream(context.Context, string) (bool, error)
	Hangup(context.Context) error

	// RemoteDialogEnded drops the dialog the peer has already torn down,
	// without sending a BYE back. The session calls it on exactly the paths
	// where it skips Hangup, so the SIP layer is never left holding a dialog
	// nothing will ever end. See SourceSession.RemoteDialogEnded.
	RemoteDialogEnded()
}
```

- [ ] **Step 5: Add the field and reset it per start**

In `internal/media/source_session.go`, add to the `SourceSession` struct after `terminating  bool` (line 63):

```go
	// ownsDialog records whether StartStream created the SIP dialog. False for a
	// lease adopted for an answered inbound call, whose dialog belongs to the
	// call: no teardown of this session's media may end it.
	ownsDialog bool
```

In `Start`, alongside the existing per-start resets at lines 89-90, add:

```go
	s.ownsDialog = false
```

- [ ] **Step 6: Consume the value at the call site**

In `internal/media/source_session.go`, add `ownsDialog` to the local declaration at line 95:

```go
	var videoStarted, audioStarted, sipStarted, ownsDialog bool
```

Replace lines 165-169:

```go
	ownsDialog, err := s.sip.StartStream(startCtx, s.sourceConfig.DevAddr)
	if err != nil {
		return fmt.Errorf("start outgoing sip: %w", err)
	}

	sipStarted = true

	s.mu.Lock()
	s.ownsDialog = ownsDialog
	s.mu.Unlock()
```

`:=` is correct here even though `ownsDialog` already exists: it and the `var` line are both at `Start`'s top-level scope, so `ownsDialog` is assigned rather than shadowed, and `err` — which is only ever declared inside `if` statements above — is newly declared. The deferred cleanup in Task 2 reads the same local.

- [ ] **Step 7: Update the media test fakes**

Replace `internal/media/source_session_test.go:354-374`:

```go
// adopted makes StartStream report that it created no dialog, which is what the
// manager does during an answered inbound call. It defaults to false, so every
// test that does not opt in stands for an outbound preview that owns its dialog.
type fakeSourceSIP struct {
	startCalls   int
	hangups      int
	remoteEndeds int
	adopted      bool
}

func (s *fakeSourceSIP) StartStream(context.Context, string) (bool, error) {
	s.startCalls++

	return !s.adopted, nil
}

func (s *fakeSourceSIP) Hangup(context.Context) error { s.hangups++; return nil }
func (s *fakeSourceSIP) RemoteDialogEnded()           { s.remoteEndeds++ }

type blockingInviteSIP struct{ started chan struct{} }

func (s *blockingInviteSIP) StartStream(ctx context.Context, _ string) (bool, error) {
	close(s.started)
	<-ctx.Done()

	return false, fmt.Errorf("wait for invite answer: %w", ctx.Err())
}

func (*blockingInviteSIP) Hangup(context.Context) error { return nil }
func (*blockingInviteSIP) RemoteDialogEnded()           {}
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./internal/signaling/ -run TestManager_StartStream -v`
Expected: PASS, two tests.

Run: `go build ./... && go test ./internal/media/ ./internal/signaling/`
Expected: no build output; both packages `ok`.

If another package fails to build on `StartStream`, it is calling the manager's method. Fix it by discarding the new value with `_, err := ...` — no other caller needs the ownership fact.

- [ ] **Step 9: Commit**

```bash
git add internal/signaling/manager.go internal/signaling/manager_test.go internal/media/source_session.go internal/media/source_session_test.go
git commit -m "feat(signaling): report whether StartStream created the dialog"
```

---

### Task 2: A failed startup never BYEs a dialog it did not create

**Files:**
- Modify: `internal/media/source_session.go:100-112` (the deferred cleanup inside `Start`)
- Test: `internal/media/source_session_test.go`

**Interfaces:**
- Consumes: `SourceSession.ownsDialog` and `fakeSourceSIP.adopted` from Task 1.
- Produces: nothing for later tasks.

This is the BYE site that a teardown-reason check could never reach: it runs from inside `Start`, before any teardown reason exists.

- [ ] **Step 1: Write the failing test**

Append to `internal/media/source_session_test.go`:

```go
func TestSourceSessionFailedStartupDoesNotBYEAnAdoptedCall(t *testing.T) {
	sip := &fakeSourceSIP{adopted: true}
	av := &fakeSourceAV{err: errors.New("nack")}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}
	session := NewSourceSession(nil, SourceConfig{Model: "C100X", DevAddr: "20"}, "main", sip, av, video, audio)

	if err := session.Start(context.Background()); err == nil {
		t.Fatal("Start() error = nil, want the AV failure")
	}

	if sip.hangups != 0 {
		t.Fatalf("BYE count = %d, want 0 — the dialog belongs to the answered call, not to this session", sip.hangups)
	}

	if video.closes != 1 || audio.closes != 1 {
		t.Fatalf("receivers not released: video=%#v audio=%#v", video, audio)
	}
}
```

`TestSourceSessionCleansUpSIPAndReceiversWhenAVFails` at line 46 is the owned-dialog counterpart and must keep passing untouched: it asserts `sip.hangups != 1` is a failure.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/media/ -run TestSourceSessionFailedStartupDoesNotBYEAnAdoptedCall -v`
Expected: FAIL with `BYE count = 1, want 0`.

- [ ] **Step 3: Gate the startup BYE on ownership**

In `internal/media/source_session.go`, replace line 102:

```go
			hangup := sipStarted && !s.remoteEnded && !s.terminating
```

with:

```go
			// ownsDialog is what keeps a failed startup from ending a call it
			// merely borrowed. On a lease adopted for an answered inbound call no
			// INVITE went out, so there is no dialog of ours to BYE — and the one
			// that exists is the call the user is on.
			hangup := sipStarted && ownsDialog && !s.remoteEnded && !s.terminating
```

Leave the `remoteEnded` line below it unchanged: when the peer really ended the dialog, the SIP layer must be told whether or not the dialog was ours.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/media/ -run TestSourceSession -v`
Expected: PASS, including `TestSourceSessionCleansUpSIPAndReceiversWhenAVFails`.

Run: `go test -race ./internal/media/`
Expected: `ok  	bticino-go-companion/internal/media`

- [ ] **Step 5: Commit**

```bash
git add internal/media/source_session.go internal/media/source_session_test.go
git commit -m "fix(media): keep a failed startup from ending an adopted call"
```

---

### Task 3: A media teardown ends the call only when the consumer released it

**Files:**
- Modify: `internal/media/stream_coordinator.go` — new type and causes near the constants at lines 21-24; `sourceEvents` at `:256-270`; `startSource` at `:304`, `:311`; `Reconcile` at `:375`; `Release` at `:384`; `stop` at `:409-437`; `finishStop` at `:439-476`; `ObserveControlStop` at `:528`
- Modify: `internal/media/source_session.go:48` (the ownership claim in the doc comment), the struct, `Start`'s resets, `Close` at `:223`
- Modify: `internal/signaling/manager.go:270-278` (the `Hangup` doc comment)
- Test: `internal/media/source_session_test.go`, `internal/media/stream_coordinator_test.go`

**Interfaces:**
- Consumes: `SourceSession.ownsDialog` from Task 1.
- Produces: `type ManagedSourceCallEnder interface { EndCallOnClose() }`, implemented by `*SourceSession`. Nothing after this task depends on it.

- [ ] **Step 1: Write the failing tests**

Append to `internal/media/source_session_test.go`:

```go
func TestSourceSessionCloseDoesNotBYEAnAdoptedCall(t *testing.T) {
	sip := &fakeSourceSIP{adopted: true}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, &fakeSourceAV{}, video, audio)

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if sip.hangups != 0 {
		t.Fatalf("BYE count = %d, want 0 — a media teardown must not end the call the user is on", sip.hangups)
	}

	if video.closes != 1 || audio.closes != 1 {
		t.Fatalf("receivers not released: video=%#v audio=%#v", video, audio)
	}
}

func TestSourceSessionCloseEndsAnAdoptedCallWhenTheConsumerReleased(t *testing.T) {
	sip := &fakeSourceSIP{adopted: true}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, &fakeSourceAV{}, &fakeSourceReceiver{}, &fakeSourceReceiver{})

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	session.EndCallOnClose()

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if sip.hangups != 1 {
		t.Fatalf("BYE count = %d, want 1 — the user let go of the lease, which ends the doorbell call on purpose", sip.hangups)
	}
}
```

Append to `internal/media/stream_coordinator_test.go`:

```go
type callEndingSource struct {
	closes       int
	endCallCalls int
}

func (*callEndingSource) Start(context.Context) error   { return nil }
func (s *callEndingSource) Close(context.Context) error { s.closes++; return nil }
func (s *callEndingSource) EndCallOnClose()             { s.endCallCalls++ }

func TestStreamCoordinatorConsumerReleaseEndsTheCall(t *testing.T) {
	source := &callEndingSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if !c.Release(lease) {
		t.Fatal("release source lease")
	}

	if source.endCallCalls != 1 {
		t.Fatalf("EndCallOnClose calls = %d, want 1", source.endCallCalls)
	}
}

func TestStreamCoordinatorOpenWebNetStopDoesNotEndTheCall(t *testing.T) {
	source := &callEndingSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	beginControlAttempt(c)
	c.ObserveControlTrack(true)
	endControlAttempt(c)
	c.ObserveControlStop()

	if source.closes != 1 {
		t.Fatalf("source closes = %d, want 1 — the media teardown itself is unchanged", source.closes)
	}

	if source.endCallCalls != 0 {
		t.Fatalf("EndCallOnClose calls = %d, want 0 — a foreign stop frame must not end the call", source.endCallCalls)
	}

	if c.Release(lease) {
		t.Fatal("released an already stopped lease")
	}
}

func TestStreamCoordinatorStalledTracksDoNotEndTheCall(t *testing.T) {
	source := &callEndingSource{}
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})

	lease, err := c.Acquire(context.Background(), config.Entrypoint{ID: "main", DevAddr: "20"}, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	beginControlAttempt(c)
	c.ObserveControlTrack(true)
	c.ObserveControlTrack(false)
	endControlAttempt(c)

	if !c.Reconcile(lease, time.Now().Add(2*trackFlowTimeout)) {
		t.Fatal("Reconcile() = false, want true — both requested tracks are long past the flow timeout")
	}

	if source.endCallCalls != 0 {
		t.Fatalf("EndCallOnClose calls = %d, want 0 — an RTP stall must not end the call", source.endCallCalls)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/media/ -run "TestSourceSessionClose|TestStreamCoordinatorConsumerRelease|TestStreamCoordinatorOpenWebNetStopDoesNot|TestStreamCoordinatorStalledTracksDoNot" -v`
Expected: FAIL to build, with `session.EndCallOnClose undefined (type *SourceSession has no field or method EndCallOnClose)`.

- [ ] **Step 3: Add the typed cause and the capability interface**

In `internal/media/stream_coordinator.go`, after the `const` block at lines 21-24, add:

```go
// stopCause says why a lease is being torn down, and whether that teardown may
// end the SIP call behind it.
//
// Only a consumer letting go of the lease ends the call: without a WebRTC
// session there is no audio path to the user, so a call left up would leave the
// visitor talking to nobody. Every other cause is a media-layer misfortune — a
// foreign OpenWebNet stop frame, an RTP stall, a transport failure, a failed
// startup — and the user may still be mid-conversation, so the call must
// survive it and the card can re-offer to get the picture back.
//
// endsCall is false in the zero value on purpose: a cause added later cannot
// end a call unless it says so. See
// docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md.
type stopCause struct {
	reason   string
	endsCall bool
}

var (
	causeConsumerReleased = stopCause{reason: "consumer released", endsCall: true}
	causeRemoteBYE        = stopCause{reason: "remote sip bye"}
	causeControlStop      = stopCause{reason: "openwebnet stream stop"}
	causeTracksStalled    = stopCause{reason: "rtp tracks stalled"}
	causeSourceFailure    = stopCause{reason: "managed source failure"}
	causeStartupFailed    = stopCause{reason: "source startup failed"}
	causeStartupTimedOut  = stopCause{reason: "source startup timed out"}
)

// ManagedSourceCallEnder is an optional capability of a managed source that
// carries a SIP call it did not create. Such a source keeps the call up through
// its own teardown unless finishStop tells it otherwise, which it does only for
// a teardown a consumer asked for.
type ManagedSourceCallEnder interface {
	EndCallOnClose()
}
```

- [ ] **Step 4: Thread the cause through `stop` and `finishStop`**

In `internal/media/stream_coordinator.go`, change the two signatures and the five log sites. Replace lines 409-476 in full:

```go
func (c *StreamCoordinator) stop(lease *StreamLease, cause stopCause) bool {
	if lease == nil {
		return false
	}

	c.mu.Lock()
	if c.leaseID != lease.id {
		c.mu.Unlock()
		c.logger.Debug("stream stop ignored; lease is no longer active", "lease_id", lease.id, "reason", cause.reason)

		return false
	}

	if c.stopping {
		c.mu.Unlock()
		c.logger.Debug("stream stop ignored; teardown already in progress", "lease_id", lease.id, "reason", cause.reason)

		return false
	}

	c.stopping = true
	source, cleanup := c.source, c.cleanup
	c.source, c.cleanup = nil, nil
	c.mu.Unlock()
	c.logger.Info("stream teardown started", "lease_id", lease.id, "reason", cause.reason)
	c.finishStop(lease, source, cleanup, cause)

	return true
}

func (c *StreamCoordinator) finishStop(lease *StreamLease, source ManagedSource, cleanup func(), cause stopCause) {
	var closeErr error

	if source != nil {
		// Told before Close, because Close is where the source decides whether to
		// send a BYE. A source that did not create its dialog keeps the call up
		// unless this ran.
		if cause.endsCall {
			if ender, ok := source.(ManagedSourceCallEnder); ok {
				ender.EndCallOnClose()
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		closeErr = source.Close(ctx)

		cancel()
	}

	if cleanup != nil {
		cleanup()
	}

	c.mu.Lock()
	if c.leaseID == lease.id {
		c.leaseID = 0
		c.controlLeaseID = 0
		c.starting = false
		c.stopping = false
		c.snapshot = StreamSnapshot{Owner: StreamOwnerIdle}
	}

	snapshot := c.snapshot
	observer := c.observer
	c.mu.Unlock()

	if observer != nil {
		observer(snapshot)
	}

	if closeErr != nil {
		c.logger.Error("stream teardown failed", "lease_id", lease.id, "reason", cause.reason, "error", fmt.Errorf("close managed source: %w", closeErr))
		return
	}

	c.logger.Info("stream teardown completed", "lease_id", lease.id, "reason", cause.reason)
}
```

- [ ] **Step 5: Update every `stop` call site**

Six edits in `internal/media/stream_coordinator.go`. Each replaces the string argument with the matching cause:

| Line | Was | Becomes |
|---|---|---|
| `:257` | `c.stop(lease, "remote sip bye")` | `c.stop(lease, causeRemoteBYE)` |
| `:265` | `c.stop(lease, "managed source failure")` | `c.stop(lease, causeSourceFailure)` |
| `:304` | `c.stop(lease, "source startup failed")` | `c.stop(lease, causeStartupFailed)` |
| `:311` | `go c.stop(lease, "source startup timed out")` | `go c.stop(lease, causeStartupTimedOut)` |
| `:375` | `c.stop(lease, "rtp tracks stalled")` | `c.stop(lease, causeTracksStalled)` |
| `:528` | `c.stop(lease, "openwebnet stream stop")` | `c.stop(lease, causeControlStop)` |

And `Release` at `:383-385`:

```go
// Release ends the lease and, with it, the SIP call behind it. That is the one
// teardown the user asked for.
func (c *StreamCoordinator) Release(lease *StreamLease) bool {
	return c.stop(lease, causeConsumerReleased)
}
```

- [ ] **Step 6: Add the opt-in to `SourceSession` and gate `Close`**

In `internal/media/source_session.go`, add to the struct after the `ownsDialog` field from Task 1:

```go
	// endCallOnClose lets a teardown the consumer asked for end a dialog this
	// session did not create. Nothing else may.
	endCallOnClose bool
```

Add `s.endCallOnClose = false` to `Start`'s per-start resets, beside `s.ownsDialog = false`.

Add this method after `SetStartedCallback` (which ends at line 198):

```go
// EndCallOnClose tells the session that the teardown about to run came from the
// consumer letting go of the lease, so the next Close must end the SIP call even
// for a dialog this session did not create.
//
// Releasing the lease ending the doorbell call is deliberate: without a WebRTC
// session there is no audio path to the user, so a call left up would leave the
// visitor talking to nobody. What must not end the call is a media teardown
// under a consumer that is still there — a foreign OpenWebNet stop frame, an
// RTP stall, a transport failure. See
// docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md.
func (s *SourceSession) EndCallOnClose() {
	s.mu.Lock()
	s.endCallOnClose = true
	s.mu.Unlock()
}
```

Replace line 223:

```go
	hangup := !s.remoteEnded && !s.terminating
```

with:

```go
	hangup := !s.remoteEnded && !s.terminating && (s.ownsDialog || s.endCallOnClose)
```

- [ ] **Step 7: Correct the two comments that now say the opposite**

Replace `internal/media/source_session.go:48`:

```go
// SourceSession owns the SIP dialog and both RTP sockets for one device source.
```

with:

```go
// SourceSession owns both RTP sockets for one device source, and owns the SIP
// dialog only when it created it. A lease adopted for an answered inbound call
// borrows the call's dialog: see ownsDialog, and
// docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md.
```

Replace the `Hangup` doc comment at `internal/signaling/manager.go:270-278`:

```go
// Hangup is idempotent: tearing down a call that is already gone is not an
// error, because SourceSession.Close runs it again on every normal teardown.
//
// It deliberately has no counterpart to StartStream's no-op on an answered
// inbound call: media.SourceSession.Close runs Hangup on every lease teardown,
// and that ends a live doorbell call on purpose. Without the WebRTC session
// there is no audio path to the user, so keeping the SIP call up would leave the
// visitor talking to nobody. The asymmetry with StartStream — which does nothing
// rather than dial while such a call is up — is intended.
```

with:

```go
// Hangup is idempotent: tearing down a call that is already gone is not an
// error, because SourceSession.Close may run it again on a later teardown.
//
// It has no counterpart to StartStream's no-op on an answered inbound call, and
// needs none: the decision now lives with the caller. Releasing the lease still
// ends a live doorbell call on purpose, because without a WebRTC session there
// is no audio path and the visitor would talk to nobody. But a teardown the
// consumer did not ask for — a foreign OpenWebNet stop frame, an RTP stall, a
// transport failure — no longer reaches here at all: SourceSession.Close skips
// Hangup for a dialog it did not create unless EndCallOnClose said otherwise.
// Ending the call for those was the defect fixed in
// docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md.
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./internal/media/ -v`
Expected: PASS. Every pre-existing test must still pass — they stand for the outbound preview, where `ownsDialog` is true.

Run: `go build ./... && go test -race ./...`
Expected: no build output; every package `ok` or `no test files`, no `FAIL`.

- [ ] **Step 9: Lint**

Run: `golangci-lint run`
Expected: no findings. If it is not installed, run `go vet ./...` and expect no output.

- [ ] **Step 10: Commit**

```bash
git add internal/media/stream_coordinator.go internal/media/stream_coordinator_test.go internal/media/source_session.go internal/media/source_session_test.go internal/signaling/manager.go
git commit -m "fix(media): let only a consumer release end the call"
```

---

### Task 4: A BYE on an answered inbound call releases the media lease

**Files:**
- Modify: `internal/signaling/dialer.go:291-313` (the `answered` branch of the BYE handler)
- Test: `internal/signaling/dialer_test.go`, `internal/signaling/manager_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks. It reuses `streamDialer.notifyRemoteDialogEnded` and the media wiring already in `internal/app/run.go:673-680`.
- Produces: nothing.

- [ ] **Step 1: Write the failing tests**

Append to `internal/signaling/dialer_test.go`:

```go
// The lease an answered inbound call holds is released by nothing else. Before
// the media hook fired here, the card kept its last frame until an OpenWebNet
// stop frame or the stall watchdog reclaimed the lease.
func TestStreamDialerAnsweredInboundByeReleasesTheMediaLease(t *testing.T) {
	handler := newRingingInboundHandler()
	dialer := testInboundDialer(t, handler)

	released := make(chan struct{})
	dialer.SetRemoteDialogEnded(func() { close(released) })

	invite := testInviteRequest()
	recorder := siptest.NewServerTxRecorder(invite)

	returned := make(chan struct{})

	go func() {
		defer close(returned)

		dialer.onInvite(invite, recorder)
	}()

	dialog := waitRinging(t, handler)
	session := serverSessionOf(t, dialog)

	answered := make(chan error, 1)

	go func() { answered <- dialog.Respond(context.Background(), 200, "OK", "v=0\r\n") }()

	waitDialogState(t, session, sip.DialogStateEstablished)

	ack := testAckRequest(session)
	dialer.onAck(ack, siptest.NewServerTxRecorder(ack))

	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("Respond(200) error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the answer never completed after the ACK")
	}

	bye := testByeRequest(session)
	dialer.onBye(bye, siptest.NewServerTxRecorder(bye))

	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("the media hook never fired for a BYE on an answered inbound call: the lease would be left outstanding")
	}

	recorder.Terminate()
	waitClosed(t, returned, "onInvite() did not return once its INVITE transaction had retired")
}
```

Append to `internal/signaling/manager_test.go`:

```go
// TestManager_OnInviteRespondsBusyWhileADialogIsActive pins the invariant that
// the media teardown on a remote BYE relies on: an outbound preview and an
// answered inbound call cannot coexist, so notifying the media hook from the
// dialer's BYE handler cannot reach an unrelated preview's lease. If this test
// is ever changed, revisit that branch in internal/signaling/dialer.go.
func TestManager_OnInviteRespondsBusyWhileADialogIsActive(t *testing.T) {
	t.Parallel()

	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))
	if _, err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatal(err)
	}

	dialog := &fakeIncomingDialog{id: testDialogID}
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if len(dialog.responses) != 1 || dialog.responses[0].status != 486 {
		t.Fatalf("responses = %#v, want a single 486 Busy Here", dialog.responses)
	}

	if err := manager.Answer(context.Background()); !errors.Is(err, ErrNoIncomingDialog) {
		t.Fatalf("Answer() error = %v, want ErrNoIncomingDialog — the refused INVITE was never reserved", err)
	}
}
```

486 is what `respondBusy` sends (`internal/signaling/manager.go:180`). `errors` is already imported in this file.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/signaling/ -run "TestStreamDialerAnsweredInboundByeReleasesTheMediaLease|TestManager_OnInviteRespondsBusyWhileADialogIsActive" -v`
Expected: `TestStreamDialerAnsweredInboundByeReleasesTheMediaLease` FAILS with "the media hook never fired". `TestManager_OnInviteRespondsBusyWhileADialogIsActive` should already PASS — it pins behaviour that exists today.

- [ ] **Step 3: Notify the media hook, and rewrite the comment that forbade it**

In `internal/signaling/dialer.go`, replace lines 291-313:

```go
	if answered {
		// Two notifications, and both are needed.
		//
		// The handler is the manager: it clears the active call without sending a
		// BYE back, because the far end has already sent one.
		//
		// The callback is the media source's, and it was deliberately omitted here
		// until inbound answering could hold a media lease: the hook meant "the
		// preview's own dialog went away", so firing it would have torn down media
		// for a call the user was still on. An answered call can now hold the
		// lease, and nothing else releases it — without this the card keeps its
		// last frame until an OpenWebNet stop frame or the stall watchdog reclaims
		// it.
		//
		// It cannot reach an unrelated preview's lease: OnInvite answers busy
		// while any dialog is active, so a preview and an answered inbound call
		// cannot coexist. TestManager_OnInviteRespondsBusyWhileADialogIsActive
		// pins that invariant. See
		// docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md.
		//
		// The manager call is synchronous, which is safe because this goroutine is
		// already tracked by inboundWG and RemoteDialogEnded neither blocks nor
		// re-enters the dialer: it takes the manager's state lock, clears the
		// active dialog and queues the event for the manager's own drain
		// goroutine. The media callback runs on a goroutine of its own and reaches
		// the manager a second time, which is harmless: RemoteDialogEnded returns
		// early when nothing is active, and only the first call publishes
		// CallHungUp.
		if handler := d.inboundHandler(); handler != nil {
			handler.RemoteDialogEnded()
		}

		d.notifyRemoteDialogEnded()

		d.logger.Info("inbound sip call ended by remote", "dialog_id", inboundDialogID(dialog))

		return
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/signaling/ -run "TestStreamDialer|TestManager_" -v`
Expected: PASS.

Run: `go test -race ./...`
Expected: every package `ok` or `no test files`, no `FAIL`.

- [ ] **Step 5: Lint**

Run: `golangci-lint run`
Expected: no findings. If it is not installed, run `go vet ./...` and expect no output.

- [ ] **Step 6: Commit**

```bash
git add internal/signaling/dialer.go internal/signaling/dialer_test.go internal/signaling/manager_test.go
git commit -m "fix(signaling): release media when the peer ends an answered call"
```

---

### Task 5: The flexisip provisioning defects

**Files:**
- Modify: `scripts/install.sh:209-303` (`FLEXISIP_ROUTE_INT`, `route_int_has_companion`, `provision_flexisip_user`)

**Interfaces:**
- Consumes: nothing. Independent of Tasks 1-4 and may be done in any order relative to them.
- Produces: nothing.

`scripts/` has no test harness, so this task is verified by reading the diff and by a run on the device. Keep the change small enough to read.

- [ ] **Step 1: Point the group edit at the file flexisip reads**

In `scripts/install.sh`, delete the now-unused variable at line 211:

```sh
FLEXISIP_ROUTE_INT="/etc/flexisip/users/route_int.conf"
```

Rename `route_int_has_companion` to `route_has_companion` and retarget its comment. Replace lines 239-248:

```sh
# route_has_companion reports whether the companion contact is in the alluser
# group of $2, not merely somewhere in the file. Anchoring on the alluser contact
# and refusing a '#' in between is what rejects a match that sits in a trailing
# comment: the insertion below appends to the end of the line, so on a line such
# as '<sip:alluser@dom> <sip:x@dom>   # handsets' it lands after the '#' and
# changes nothing, while a plain grep for the contact finds it there all the same
# and would report a file that was never provisioned as provisioned.
route_has_companion() {
	grep -q "sip:alluser@$1>[^#]*sip:${SIP_USER}@$1" "$2"
}
```

- [ ] **Step 2: Rewrite the provisioning body**

Replace `scripts/install.sh:250-303` (`provision_flexisip_user`) in full:

```sh
provision_flexisip_user() {
	domain="$1"
	# route_int.conf is deliberately absent: flexisip reads
	# static-records-file=/etc/flexisip/users/route.conf, so editing route_int.conf
	# provisioned nothing. Requiring a file this installer no longer writes would
	# fail an installation for no reason.
	for file in "${FLEXISIP_USERS_DB}" "${FLEXISIP_ROUTE}"; do
		if ! backup_once "${file}"; then
			warn "Missing ${file}; skipping SIP inbound provisioning."
			return 1
		fi
	done

	if ! grep -q "^${SIP_USER}@${domain} " "${FLEXISIP_USERS_DB}"; then
		hash_line="$(awk -v d="@${domain}" 'index($1, d) { print $2; exit }' "${FLEXISIP_USERS_DB}" || true)"
		if [ -z "${hash_line}" ]; then
			warn "No existing user hash found in ${FLEXISIP_USERS_DB}; skipping SIP inbound provisioning."
			return 1
		fi
		printf '%s@%s %s ;\n' "${SIP_USER}" "${domain}" "${hash_line}" >> "${FLEXISIP_USERS_DB}"
		log "Added ${SIP_USER}@${domain} to users.db.txt"
	fi

	# No static record is written. One used to be, pointing at <sip:127.0.0.1> —
	# which resolves to flexisip itself rather than the companion's listener.
	# Dynamic registration supplies a correct contact, so the record added nothing
	# and made flexisip fork every inbound INVITE twice, delivering every doorbell
	# call to the companion in duplicate. Devices provisioned before this change
	# keep that line until it is removed by hand; see
	# docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md.

	if route_has_companion "${domain}" "${FLEXISIP_ROUTE}"; then
		ok "SIP inbound already provisioned."
		return 0
	fi

	tmp="${FLEXISIP_ROUTE}.companion.tmp"
	sed "s|\(<sip:alluser@${domain}>.*\)$|\1, <sip:${SIP_USER}@${domain}>|" "${FLEXISIP_ROUTE}" > "${tmp}"

	# route.conf must stay one line per group; a mangled file breaks the whole
	# intercom, so validate before replacing the original. The count is compared
	# as a string: grep -c prints 0 and exits 1 with no match, and an empty count
	# must take the warn branch rather than make [ ] error out.
	#
	# The second half of the check has to look at where the contact landed, not
	# only whether it is there: the substitution above appends to the end of the
	# line, which on a line carrying a trailing comment is inside the comment. A
	# plain grep passes that, and the installer then reports a provisioned file
	# for a change that does nothing.
	alluser_lines="$(grep -c "sip:alluser@${domain}" "${tmp}" || true)"
	if [ "${alluser_lines}" != "1" ] || ! route_has_companion "${domain}" "${tmp}"; then
		rm -f "${tmp}"
		warn "route.conf rewrite failed validation; original left untouched."
		return 1
	fi

	mv "${tmp}" "${FLEXISIP_ROUTE}"
	log "Added ${SIP_USER}@${domain} to the alluser group"

	return 0
}
```

- [ ] **Step 3: Check for stale references**

Run: `grep -n "FLEXISIP_ROUTE_INT\|route_int_has_companion\|route_int.conf" scripts/install.sh`
Expected: no output. Any hit is a reference left behind — fix it.

- [ ] **Step 4: Check the script still parses**

Run: `sh -n scripts/install.sh`
Expected: no output.

If `shellcheck` is available, run: `shellcheck scripts/install.sh`
Expected: no new findings relative to before the change. Pre-existing findings elsewhere in the file are not this task's business.

- [ ] **Step 5: Verify the provisioning on the device, twice**

The spec requires idempotency, and the second run is what proves the anchored check replaced the removed static-record check correctly. On the device, back up the real files first:

```sh
cp -p /etc/flexisip/users/route.conf /tmp/route.conf.before
```

Run the installer, then inspect:

```sh
grep -n 'alluser' /etc/flexisip/users/route.conf
grep -c '<sip:127\.0\.0\.1>' /etc/flexisip/users/route.conf
```

Expected: the companion contact appended to the single `alluser` line, and a count of `0` static records — the installer must not have written one. `route_int.conf` must be byte-identical to before.

Run the installer a second time. Expected: it logs `SIP inbound already provisioned.` and `route.conf` is unchanged from after the first run:

```sh
diff /tmp/route.conf.after /etc/flexisip/users/route.conf
```

(capture `/tmp/route.conf.after` between the two runs). Any diff on the second run is a failure of the anchored check.

- [ ] **Step 6: Commit**

```bash
git add scripts/install.sh
git commit -m "fix(install): provision the companion in the route file flexisip reads"
```

---

### Task 6: On-device verification

**Files:** none. This task changes no code. It is the gate that says the fix works on real hardware.

**Interfaces:**
- Consumes: a deployed binary containing Tasks 1 to 4.

- [ ] **Step 1: Deploy and restart**

Follow `docs/build-and-install.md` for the cross-build and copy, then:

```sh
/etc/init.d/companion restart
```

- [ ] **Step 2: Ring, answer from the card, and let the internal unit time out**

Press the doorbell at the outdoor station, answer from the Home Assistant card, and stay in the call — long enough for the physical internal unit's ringing leg to time out, which is the trigger observed on 2026-08-06.

- [ ] **Step 3: Confirm the call survived**

The call must still be up after the internal unit stops ringing. The picture may or may not drop; that is section 4 of the spec and is not what this step tests.

In the companion log, expect a `stream teardown started` whose `reason` is **not** `consumer released`, and no BYE for the inbound dialog. What must not appear is the call ending on its own.

- [ ] **Step 4: Confirm a consumer release still ends the call**

Ring, answer from the card, then hang up from the card. The doorbell call must end. This is the behaviour Task 3 deliberately preserved, and the one most at risk from the change.

- [ ] **Step 5: Confirm the outbound preview is unchanged**

With no call in progress, open the preview from the card and close it. It must start and stop exactly as before.

- [ ] **Step 6: Confirm a remote hangup clears the card**

Ring, answer from the card, then hang up at the doorbell station. The card must stop showing the last frame promptly rather than holding it for the several seconds it took before Task 4.

- [ ] **Step 7: If the picture drops, capture the evidence for the deferred work**

Section 7 of the spec has the two measurements that unblock the remaining improvements. Note that the AV capture command in the 2026-08-02 plan is stale — commit d8a7d4b moved the receivers to ephemeral ports, so use:

```sh
tcpdump -i lo -n udp
```

Record whether RTP to the companion's ports stops when the internal unit times out, and which frame appears in `GET /webui/api/logs/openwebnet` at that moment (`*7*0*##` or `*7*9**##`). Do not fix it here.

---

## Out of scope

Recorded in section 9 of the spec, and deliberately absent from this plan:

- Confirming an OpenWebNet stop frame against RTP flow before tearing down. It needs measurement 1 in section 7 of the spec to be worth building, and it cannot help at all if the AV is freed bus-wide.
- Telling the intercom that a call was answered elsewhere, so the internal unit stops ringing and the second AV consumer never exists. It needs measurement 2 — the frame is not in the vocabulary the code models, and guessing is unsafe.
- Removing the bad static record from already-installed devices. The test device was corrected by hand on 2026-08-06.

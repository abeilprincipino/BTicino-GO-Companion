# Intercom Video While Ringing — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the Home Assistant card show the intercom video while the doorbell rings and keep that same session through the answer, so the answer is instant.

**Architecture:** A WebRTC lease taken while the inbound call rings becomes a *ringing preview*: the source borrows the ringing call without dialling (`Manager.AdoptRingingCall`) and asks the intercom for video only. `CallAnswered` promotes it in place (audio AV request on the already-bound port, backchannel unblocked); the end of the ring stops it without touching the call. The media path can no longer produce a 603: `SourceSIP` only has `HangupCall`, which sends a BYE on an active dialog and does nothing to a ringing one.

**Tech Stack:** Go 1.x (companion, pion/webrtc, sipgo), vanilla JS custom element (card, `BTicinoGO-Integration`).

**Spec:** `docs/superpowers/specs/2026-10-15-ringing-preview-design.md`

## Global Constraints

- `internal/media` tests do not run on Windows. For `internal/media` (and `internal/app` if it does not run either) verify with `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/` (type-checks the tests) and `GOOS=linux GOARCH=arm GOARM=7 go build ./...`. Never cross-compile into WSL to run them.
- `internal/signaling` and `internal/openwebnet` tests run on Windows: `go test ./internal/signaling/ ./internal/openwebnet/`.
- The uncommitted changes in `internal/signaling/dialer.go`, `internal/signaling/dialer_test.go` and `dist/companion` are not part of this work: never `git add` them. Stage files by explicit path only.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Invariant to preserve in every task: **before `CallAnswered`, no path of a media session sends a BYE or a 603.**
- Comments follow the repo's style: explain *why*, reference the spec path when a rule comes from it.

## Deviation from the spec, decided while planning

Spec 4.3 says `Manager.StartStream` returns `(false, nil)` with a ringing call. The plan keeps `StartStream` refusing (`ErrIncomingDialog`) and adds `Manager.AdoptRingingCall` instead, used only by a session prepared as a ringing preview. Reason: if `StartStream` adopted a ringing call, an RTSP or HomeKit source racing past the coordinator's gate would silently attach to the ring; with a separate method only the preview can. Task 1 updates the spec accordingly.

## File map

| File | Change |
|---|---|
| `internal/signaling/manager.go` | `HangupCall`, `AdoptRingingCall`, `HasRingingInboundCall`, `ErrNoInboundCall`; `Hangup` shares a BYE helper |
| `internal/signaling/manager_test.go` | tests for the three methods |
| `internal/openwebnet/av.go` | `StartVideo`, `StartAudio`; `Start` built on them |
| `internal/openwebnet/av_test.go` | tests for the two methods |
| `internal/media/source_session.go` | `SourceSIP` gets `HangupCall` + `AdoptRingingCall` (drops `Hangup`); `SourceAV` gets `StartVideo` + `StartAudio`; ringing-preview mode, `Answered`, `Ringing` |
| `internal/media/source_session_test.go` | fakes updated; preview tests incl. one against the real `signaling.Manager` |
| `internal/media/stream_coordinator.go` | ringing probe, `AcquireRingingPreview`, `ErrRingingCall`, `ErrRingingEnded`, `ManagedSourceRingingPreview`, `PromoteRinging`, `StopRinging`, backchannel gate |
| `internal/media/stream_coordinator_test.go` | preview tests |
| `internal/media/webrtc.go` | acquire with `AcquireRingingPreview` |
| `internal/app/run.go` | probe wiring, `routeRingingPreview`, `bridgeSource` forwarders |
| `internal/app/run_test.go` | `routeRingingPreview` test |
| `BTicinoGO-Integration/custom_components/bticino_companion/www/bticino-go-intercom-card.js` | preview on ring, answer on the same session |

---

### Task 1: Manager — a media hangup never rejects a ringing call

**Files:**
- Modify: `internal/signaling/manager.go:13-17` (errors), `:270-325` (`Hangup`), after `:369` (`HasAnsweredInboundCall`)
- Test: `internal/signaling/manager_test.go` (append at end)
- Modify: `docs/superpowers/specs/2026-10-15-ringing-preview-design.md` (section 4.3)

**Interfaces:**
- Produces:
  - `var ErrNoInboundCall = errors.New("sip: no inbound call to adopt")`
  - `func (m *Manager) HangupCall(ctx context.Context) error` — BYE on the active dialog (inbound answered or outbound preview); nil and no SIP traffic when only ringing or idle.
  - `func (m *Manager) AdoptRingingCall() (answered bool, err error)` — `(false, nil)` while ringing, `(true, nil)` for an answered inbound call, `ErrNoInboundCall` otherwise. Never dials.
  - `func (m *Manager) HasRingingInboundCall() bool`

- [ ] **Step 1: Write the failing tests** — append to `internal/signaling/manager_test.go`:

```go
func TestManager_HangupCallLeavesARingingCallAlone(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.HangupCall(context.Background()); err != nil {
		t.Fatalf("HangupCall() error = %v, want nil", err)
	}

	// Only the 180. A 603 here would end every fork of the doorbell call, the
	// indoor unit's included.
	if len(dialog.responses) != 1 || dialog.responses[0].status != 180 {
		t.Fatalf("responses = %#v, want only 180 Ringing", dialog.responses)
	}

	if !manager.HasRingingInboundCall() {
		t.Fatal("HasRingingInboundCall() = false, want the call still ringing")
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatalf("Answer() after HangupCall error = %v, want the call still answerable", err)
	}
}

func TestManager_HangupCallByesAnAnsweredCall(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := manager.HangupCall(context.Background()); err != nil {
		t.Fatalf("HangupCall() error = %v", err)
	}

	if dialog.byes != 1 {
		t.Fatalf("bye count = %d, want 1", dialog.byes)
	}

	if manager.HasAnsweredInboundCall() {
		t.Fatal("HasAnsweredInboundCall() = true after HangupCall, want false")
	}
}

func TestManager_HangupCallByesAnOutgoingPreview(t *testing.T) {
	t.Parallel()

	dialer := &fakeDialer{}
	manager := NewManager("192.0.2.10", dialer, &syncEventSink{}, testResolver("main", "21"))

	if _, err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatal(err)
	}

	if err := manager.HangupCall(context.Background()); err != nil {
		t.Fatalf("HangupCall() error = %v", err)
	}

	if leaked := dialer.unbyedDialogs(); len(leaked) != 0 {
		t.Fatalf("unbyed outbound dialogs = %v, want none", leaked)
	}
}

func TestManager_HangupCallOnIdleIsANoop(t *testing.T) {
	t.Parallel()

	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))

	if err := manager.HangupCall(context.Background()); err != nil {
		t.Fatalf("HangupCall() on idle manager error = %v, want nil", err)
	}
}

func TestManager_AdoptRingingCallFollowsTheInboundCall(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	dialer := &fakeDialer{}
	manager := NewManager("192.0.2.10", dialer, &syncEventSink{}, testResolver("main", "21"))

	if _, err := manager.AdoptRingingCall(); !errors.Is(err, ErrNoInboundCall) {
		t.Fatalf("AdoptRingingCall() idle error = %v, want ErrNoInboundCall", err)
	}

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	answered, err := manager.AdoptRingingCall()
	if err != nil || answered {
		t.Fatalf("AdoptRingingCall() while ringing = (%v, %v), want (false, nil)", answered, err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	answered, err = manager.AdoptRingingCall()
	if err != nil || !answered {
		t.Fatalf("AdoptRingingCall() after Answer = (%v, %v), want (true, nil)", answered, err)
	}

	if err := manager.HangupCall(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.AdoptRingingCall(); !errors.Is(err, ErrNoInboundCall) {
		t.Fatalf("AdoptRingingCall() after hangup error = %v, want ErrNoInboundCall", err)
	}

	if dialer.calls != 0 {
		t.Fatalf("dialer calls = %d, want 0 — adopting never dials", dialer.calls)
	}
}

func TestManager_AdoptRingingCallRefusesAnOutgoingPreview(t *testing.T) {
	t.Parallel()

	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))
	if _, err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.AdoptRingingCall(); !errors.Is(err, ErrNoInboundCall) {
		t.Fatalf("AdoptRingingCall() during an outgoing preview error = %v, want ErrNoInboundCall", err)
	}
}

func TestManager_HasRingingInboundCallTracksTheRing(t *testing.T) {
	t.Parallel()

	manager := NewManager("192.0.2.10", &fakeDialer{}, &syncEventSink{}, testResolver("main", "21"))

	if manager.HasRingingInboundCall() {
		t.Fatal("HasRingingInboundCall() = true with no call, want false")
	}

	if err := manager.OnInvite(context.Background(), &fakeIncomingDialog{id: testDialogID}); err != nil {
		t.Fatal(err)
	}

	if !manager.HasRingingInboundCall() {
		t.Fatal("HasRingingInboundCall() = false while ringing, want true")
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	if manager.HasRingingInboundCall() {
		t.Fatal("HasRingingInboundCall() = true after Answer, want false")
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/signaling/ -run "HangupCall|AdoptRingingCall|HasRingingInboundCall" -v`
Expected: build failure — `manager.HangupCall undefined`, `manager.AdoptRingingCall undefined`, `ErrNoInboundCall undefined`.

- [ ] **Step 3: Implement** in `internal/signaling/manager.go`.

Add the error to the `var` block:

```go
var (
	ErrNoIncomingDialog = errors.New("sip: no incoming dialog")
	ErrIncomingDialog   = errors.New("sip: an incoming dialog exists")
	ErrActiveDialog     = errors.New("sip: an active dialog exists")
	ErrNoInboundCall    = errors.New("sip: no inbound call to adopt")
)
```

Replace the whole `Hangup` function (from its doc comment through its closing brace) with `Hangup`, `HangupCall` and the shared helper:

```go
// Hangup is idempotent: tearing down a call that is already gone is not an
// error.
//
// It ends whatever call is up, a ringing one included, which it answers 603
// Decline. That is right for POST /api/v3/call/hangup, where the user is
// rejecting the call, and wrong for anything a media teardown does: a 6xx ends
// every fork of the doorbell call, the indoor unit's included. The media path
// therefore uses HangupCall, never this. See
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
func (m *Manager) Hangup(ctx context.Context) error {
	m.mu.Lock()

	if m.active != nil {
		return m.byeActiveLocked(ctx)
	}

	dialog := m.takeIncomingLocked(nil)
	if dialog == nil {
		m.mu.Unlock()

		return nil
	}

	m.publishLocked(core.CallHungUp{DialogID: dialog.ID()})

	if err := dialog.Respond(ctx, 603, "Decline", ""); err != nil {
		return fmt.Errorf("sip decline response: %w", err)
	}

	return nil
}

// HangupCall ends the active dialog with a BYE and does nothing else: a call
// that is only ringing, or no call at all, is left exactly as it is.
//
// It is the only hangup a media session can reach. A session's teardown —
// whether a consumer let go of the lease or the media failed — must never
// reject a ringing doorbell call, because the 603 would end every fork,
// including the indoor unit that is still ringing. Releasing the lease of an
// answered call still ends it on purpose: without a WebRTC session there is no
// audio path and the visitor would talk to nobody. See
// docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md and
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
func (m *Manager) HangupCall(ctx context.Context) error {
	m.mu.Lock()

	if m.active == nil {
		m.mu.Unlock()

		return nil
	}

	return m.byeActiveLocked(ctx)
}

// byeActiveLocked drops the active dialog and sends its BYE. m.mu must be held
// and m.active must be set; it releases m.mu, like publishLocked.
func (m *Manager) byeActiveLocked(ctx context.Context) error {
	active := m.active
	dialogID := m.activeID
	m.active = nil
	m.activeID = ""
	m.activeIncoming = false

	// activeID is empty exactly for an outbound preview, because StartStream
	// publishes nothing: the projector never saw a dialog, and
	// CallHungUp{DialogID: ""} would be rejected as an invalid transition.
	var event core.Event
	if dialogID != "" {
		event = core.CallHungUp{DialogID: dialogID}
	}

	m.publishLocked(event)

	if err := active.Bye(ctx); err != nil {
		return fmt.Errorf("sip bye: %w", err)
	}

	return nil
}
```

After `HasAnsweredInboundCall`, add:

```go
// HasRingingInboundCall reports whether an inbound call is ringing and not yet
// answered here. The media coordinator asks it to decide which consumers may
// take the stream during the ring: only a ringing preview may.
func (m *Manager) HasRingingInboundCall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.incoming != nil
}

// AdoptRingingCall lets a ringing preview borrow the inbound call's stream
// without dialling. It reports whether the call has already been answered,
// which tells the session to start audio too.
//
// It never creates a dialog, so the session that calls it never owns one: no
// teardown of its media can end the call. ErrNoInboundCall means the ring ended
// before the preview got here; a fresh INVITE would only come back 486 Busy
// Here from an intercom still winding the call down. See
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
func (m *Manager) AdoptRingingCall() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch {
	case m.incoming != nil:
		return false, nil
	case m.active != nil && m.activeIncoming:
		return true, nil
	default:
		return false, ErrNoInboundCall
	}
}
```

- [ ] **Step 4: Run the whole package**

Run: `go test ./internal/signaling/`
Expected: `ok` (new tests pass, `TestManager_HangupDeclinesIncomingDialog` and `TestManager_HangupIsIdempotent` still pass).

- [ ] **Step 5: Amend the spec** — in `docs/superpowers/specs/2026-10-15-ringing-preview-design.md`, section 4.3, replace the paragraph starting ``"`Manager.StartStream` with a ringing call returns `(false, nil)`"`` with:

```markdown
`Manager.StartStream` keeps refusing a ringing call (`ErrIncomingDialog`). A session prepared as a
ringing preview calls **`AdoptRingingCall`** instead: it never dials, reports whether the call is
already answered, and fails with `ErrNoInboundCall` once the ring is over. Keeping the two apart
means an RTSP or HomeKit source that races past the coordinator's gate still cannot attach to the
ring.
```

- [ ] **Step 6: Commit**

```bash
git add internal/signaling/manager.go internal/signaling/manager_test.go docs/superpowers/specs/2026-10-15-ringing-preview-design.md
git commit -m "feat(signaling): let a media session borrow a ringing call and never reject it

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: AV client — start video and audio separately

**Files:**
- Modify: `internal/openwebnet/av.go:55-78`
- Modify: `internal/media/source_session.go:37-39` (`SourceAV`)
- Modify: `internal/media/source_session_test.go:449-483` (AV fakes)
- Test: `internal/openwebnet/av_test.go`

**Interfaces:**
- Produces on `*openwebnet.AVClient` and on `media.SourceAV`:
  - `StartVideo(ctx context.Context, highRes bool, port int, probe media.FlowProbe) error`
  - `StartAudio(ctx context.Context, port int, probe media.FlowProbe) error`
  - `Start` unchanged in signature and behaviour (video then audio).

- [ ] **Step 1: Write the failing tests** — append to `internal/openwebnet/av_test.go`:

```go
func TestAVClientStartVideoSendsOnlyTheVideoFrame(t *testing.T) {
	server := newAVTestServer(t, FrameACK)
	client := newAVTestClient(server)
	video := &testFlowProbe{}
	server.onFrame = func(int) { video.flowing = true }

	if err := client.StartVideo(context.Background(), false, testAVPorts.Video, video); err != nil {
		t.Fatalf("StartVideo: %v", err)
	}

	frames := server.Frames()
	if len(frames) != 1 || frames[0] != "*7*300#127#0#0#1#61007#1*##" {
		t.Fatalf("frames = %v, want only the low-res video request", frames)
	}
}

func TestAVClientStartAudioSendsOnlyTheAudioFrame(t *testing.T) {
	server := newAVTestServer(t, FrameACK)
	client := newAVTestClient(server)
	audio := &testFlowProbe{}
	server.onFrame = func(int) { audio.flowing = true }

	if err := client.StartAudio(context.Background(), testAVPorts.Audio, audio); err != nil {
		t.Fatalf("StartAudio: %v", err)
	}

	frames := server.Frames()
	if len(frames) != 1 || frames[0] != "*7*300#127#0#0#1#61553#2*##" {
		t.Fatalf("frames = %v, want only the audio request", frames)
	}
}

func TestAVClientStartAudioBoundsRejectedAttempts(t *testing.T) {
	server := newAVTestServer(t, FrameNACK, FrameNACK, FrameNACK)
	client := newAVTestClient(server)

	err := client.StartAudio(context.Background(), testAVPorts.Audio, &testFlowProbe{})
	if !errors.Is(err, ErrAVCommandRejected) {
		t.Fatalf("StartAudio error = %v, want rejected command", err)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./internal/openwebnet/ -run "AVClientStart(Video|Audio)" -v`
Expected: build failure — `client.StartVideo undefined`.

- [ ] **Step 3: Implement** — replace `Start` in `internal/openwebnet/av.go` with:

```go
// Start requests video then audio. Each request must receive an ACK or be
// corroborated by observed RTP, and both streams must subsequently flow.
func (c *AVClient) Start(ctx context.Context, highRes bool, ports media.AVPorts, video, audio media.FlowProbe) error {
	if video == nil || audio == nil {
		return errors.New("openwebnet av flow probes are required")
	}

	if err := c.StartVideo(ctx, highRes, ports.Video, video); err != nil {
		return err
	}

	return c.StartAudio(ctx, ports.Audio, audio)
}

// StartVideo requests the video stream alone. A ringing preview starts with it:
// the intercom has no audio to give before the call is answered. See
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
func (c *AVClient) StartVideo(ctx context.Context, highRes bool, port int, probe media.FlowProbe) error {
	if probe == nil {
		return errors.New("openwebnet av video flow probe is required")
	}

	resolution := "low"
	if highRes {
		resolution = "high"
	}

	c.logger.InfoContext(ctx, "av activation starting", "video_resolution", resolution)

	return c.startStream(ctx, "video", BuildAVAddStreamVideo("127.0.0.1", port, highRes), probe)
}

// StartAudio requests the audio stream alone, on a port the session bound at
// start. It is how a ringing preview gains audio once the call is answered,
// without restarting anything.
func (c *AVClient) StartAudio(ctx context.Context, port int, probe media.FlowProbe) error {
	if probe == nil {
		return errors.New("openwebnet av audio flow probe is required")
	}

	return c.startStream(ctx, "audio", BuildAVAddStreamAudio("127.0.0.1", port), probe)
}
```

Extend `SourceAV` in `internal/media/source_session.go`:

```go
type SourceAV interface {
	Start(context.Context, bool, AVPorts, FlowProbe, FlowProbe) error
	StartVideo(context.Context, bool, int, FlowProbe) error
	StartAudio(context.Context, int, FlowProbe) error
}
```

Update the AV fakes in `internal/media/source_session_test.go` — replace `fakeSourceAV`, its `Start`, `blockingSourceAV` and `uncooperativeSourceAV` with:

```go
type fakeSourceAV struct {
	calls      int
	highRes    bool
	ports      AVPorts
	err        error
	videoCalls int
	audioCalls int
	audioPort  int
	audioErr   error
}

func (a *fakeSourceAV) Start(_ context.Context, highRes bool, ports AVPorts, _, _ FlowProbe) error {
	a.calls++
	a.highRes = highRes
	a.ports = ports

	return a.err
}

func (a *fakeSourceAV) StartVideo(_ context.Context, highRes bool, port int, _ FlowProbe) error {
	a.videoCalls++
	a.highRes = highRes
	a.ports.Video = port

	return a.err
}

func (a *fakeSourceAV) StartAudio(_ context.Context, port int, _ FlowProbe) error {
	a.audioCalls++
	a.audioPort = port

	return a.audioErr
}

type blockingSourceAV struct{ started chan struct{} }

func (a *blockingSourceAV) Start(ctx context.Context, _ bool, _ AVPorts, _, _ FlowProbe) error {
	close(a.started)
	<-ctx.Done()

	return ctx.Err()
}

func (a *blockingSourceAV) StartVideo(ctx context.Context, _ bool, _ int, _ FlowProbe) error {
	close(a.started)
	<-ctx.Done()

	return ctx.Err()
}

func (*blockingSourceAV) StartAudio(context.Context, int, FlowProbe) error { return nil }

type uncooperativeSourceAV struct {
	started chan struct{}
	release chan struct{}
}

func (a *uncooperativeSourceAV) Start(context.Context, bool, AVPorts, FlowProbe, FlowProbe) error {
	close(a.started)
	<-a.release

	return context.Canceled
}

func (a *uncooperativeSourceAV) StartVideo(context.Context, bool, int, FlowProbe) error {
	close(a.started)
	<-a.release

	return context.Canceled
}

func (*uncooperativeSourceAV) StartAudio(context.Context, int, FlowProbe) error { return nil }
```

- [ ] **Step 4: Run tests and the Linux type-check**

Run: `go test ./internal/openwebnet/`
Expected: `ok`.
Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/ ./internal/openwebnet/`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/openwebnet/av.go internal/openwebnet/av_test.go internal/media/source_session.go internal/media/source_session_test.go
git commit -m "feat(openwebnet): request the video and audio streams separately

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Source session — HangupCall only, and the ringing-preview mode

**Files:**
- Modify: `internal/media/source_session.go` (interface `SourceSIP` `:15-28`, struct `:56-81`, `Start` `:91-218`, `Close` `:243-282`, new methods)
- Test: `internal/media/source_session_test.go`

**Interfaces:**
- Consumes: `Manager.HangupCall`, `Manager.AdoptRingingCall`, `signaling.ErrNoInboundCall` (Task 1); `SourceAV.StartVideo`, `SourceAV.StartAudio` (Task 2).
- Produces:
  - `SourceSIP` = `StartStream(context.Context, string) (bool, error)`, `AdoptRingingCall() (bool, error)`, `HangupCall(context.Context) error`, `RemoteDialogEnded()`.
  - `func (s *SourceSession) PrepareRingingPreview()` — must be called before `Start`.
  - `func (s *SourceSession) Answered(ctx context.Context) error` — no-op unless started and ringing.
  - `func (s *SourceSession) Ringing() bool`

- [ ] **Step 1: Write the failing tests** — append to `internal/media/source_session_test.go` (add `"bticino-go-companion/internal/core"` to its imports):

```go
func TestSourceSessionRingingPreviewAdoptsWithoutDialingAndStartsVideoOnly(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &fakeSourceAV{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, &fakeSourceReceiver{port: 61007}, &fakeSourceReceiver{port: 61553})
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if sip.startCalls != 0 || sip.adoptCalls != 1 {
		t.Fatalf("sip start=%d adopt=%d, want 0 and 1 — a ringing preview never dials", sip.startCalls, sip.adoptCalls)
	}

	if av.videoCalls != 1 || av.calls != 0 || av.audioCalls != 0 || av.ports.Video != 61007 {
		t.Fatalf("av = %#v, want one video-only request on the bound port", av)
	}

	if !session.Ringing() {
		t.Fatal("Ringing() = false, want true")
	}
}

func TestSourceSessionRingingPreviewOfAnAnsweredCallStartsBothTracks(t *testing.T) {
	sip := &fakeSourceSIP{adoptAnswered: true}
	av := &fakeSourceAV{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, &fakeSourceReceiver{}, &fakeSourceReceiver{})
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if av.calls != 1 || av.videoCalls != 0 || session.Ringing() {
		t.Fatalf("av=%#v ringing=%v, want a full start — the call was answered before the preview got there", av, session.Ringing())
	}
}

func TestSourceSessionRingingPreviewFailsOnceTheRingIsOver(t *testing.T) {
	sip := &fakeSourceSIP{adoptErr: signaling.ErrNoInboundCall}
	av := &fakeSourceAV{}
	video, audio := &fakeSourceReceiver{}, &fakeSourceReceiver{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, video, audio)
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); !errors.Is(err, signaling.ErrNoInboundCall) {
		t.Fatalf("start error = %v, want ErrNoInboundCall", err)
	}

	if sip.startCalls != 0 || sip.hangups != 0 || av.videoCalls != 0 || av.calls != 0 {
		t.Fatalf("sip=%#v av=%#v, want no INVITE, no hangup, no AV request", sip, av)
	}

	if video.closes != 1 || audio.closes != 1 {
		t.Fatalf("receivers not released: video=%#v audio=%#v", video, audio)
	}
}

func TestSourceSessionAnsweredAsksForAudioOnTheBoundPort(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &fakeSourceAV{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, &fakeSourceReceiver{port: 61007}, &fakeSourceReceiver{port: 61553})
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := session.Answered(context.Background()); err != nil {
		t.Fatalf("Answered: %v", err)
	}

	if av.audioCalls != 1 || av.audioPort != 61553 || session.Ringing() {
		t.Fatalf("av=%#v ringing=%v, want one audio request on the port bound at start", av, session.Ringing())
	}

	if err := session.Answered(context.Background()); err != nil || av.audioCalls != 1 {
		t.Fatalf("second Answered err=%v audio calls=%d, want a no-op", err, av.audioCalls)
	}
}

func TestSourceSessionAnsweredFailureKeepsTheSessionRinging(t *testing.T) {
	sip := &fakeSourceSIP{}
	av := &fakeSourceAV{audioErr: errors.New("nack")}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", sip, av, &fakeSourceReceiver{}, &fakeSourceReceiver{})
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := session.Answered(context.Background()); err == nil {
		t.Fatal("Answered error = nil, want the audio failure")
	}

	if !session.Ringing() {
		t.Fatal("Ringing() = false after a failed promotion, want true")
	}
}

func TestSourceSessionAnsweredOnAnOrdinarySessionIsANoop(t *testing.T) {
	av := &fakeSourceAV{}
	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", &fakeSourceSIP{}, av, &fakeSourceReceiver{}, &fakeSourceReceiver{})

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := session.Answered(context.Background()); err != nil || av.audioCalls != 0 {
		t.Fatalf("Answered err=%v audio calls=%d, want a no-op", err, av.audioCalls)
	}
}

// The invariant of docs/superpowers/specs/2026-10-15-ringing-preview-design.md,
// held against the real manager: whatever the session is told, its teardown
// during the ring sends neither a BYE nor a 603.
func TestSourceSessionRingingPreviewTeardownNeverRejectsTheCall(t *testing.T) {
	dialer := &recordingStreamDialer{}
	manager := signaling.NewManager("127.0.0.1", dialer, nil, func() (core.EntrypointID, string) { return "main", "20" })
	call := &recordingIncomingDialog{id: "call-1"}

	if err := manager.OnInvite(context.Background(), call); err != nil {
		t.Fatal(err)
	}

	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", manager, &fakeSourceAV{}, &fakeSourceReceiver{}, &fakeSourceReceiver{})
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The consumer let go of the lease: the one teardown allowed to end a call.
	session.EndCallOnClose()

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if statuses := call.statuses(); len(statuses) != 1 || statuses[0] != 180 {
		t.Fatalf("responses = %v, want only 180 — a 603 would end every fork of the doorbell call", statuses)
	}

	if call.byeCount() != 0 || dialer.startCalls() != 0 {
		t.Fatalf("byes=%d invites=%d, want 0 and 0", call.byeCount(), dialer.startCalls())
	}

	if !manager.HasRingingInboundCall() {
		t.Fatal("HasRingingInboundCall() = false, want the call still ringing")
	}
}

func TestSourceSessionPromotedPreviewEndsTheCallWhenTheConsumerReleases(t *testing.T) {
	dialer := &recordingStreamDialer{}
	manager := signaling.NewManager("127.0.0.1", dialer, nil, func() (core.EntrypointID, string) { return "main", "20" })
	call := &recordingIncomingDialog{id: "call-1"}

	if err := manager.OnInvite(context.Background(), call); err != nil {
		t.Fatal(err)
	}

	session := NewSourceSession(nil, SourceConfig{Model: "C300X", DevAddr: "20"}, "main", manager, &fakeSourceAV{}, &fakeSourceReceiver{}, &fakeSourceReceiver{})
	session.PrepareRingingPreview()

	if err := session.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := session.Answered(context.Background()); err != nil {
		t.Fatalf("Answered: %v", err)
	}

	session.EndCallOnClose()

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if call.byeCount() != 1 {
		t.Fatalf("byes = %d, want 1 — the user let go of an answered call", call.byeCount())
	}
}
```

Update the SIP fakes in the same file — replace `fakeSourceSIP` (with its methods) and the `blockingInviteSIP` methods `Hangup`/`RemoteDialogEnded`:

```go
// adopted makes StartStream report that it created no dialog, which is what the
// manager does during an answered inbound call. It defaults to false, so every
// test that does not opt in stands for an outbound preview that owns its dialog.
// adoptAnswered and adoptErr drive AdoptRingingCall for ringing previews.
type fakeSourceSIP struct {
	startCalls    int
	adoptCalls    int
	hangups       int
	remoteEndeds  int
	adopted       bool
	adoptAnswered bool
	adoptErr      error
}

func (s *fakeSourceSIP) StartStream(context.Context, string) (bool, error) {
	s.startCalls++

	return !s.adopted, nil
}

func (s *fakeSourceSIP) AdoptRingingCall() (bool, error) {
	s.adoptCalls++

	return s.adoptAnswered, s.adoptErr
}

func (s *fakeSourceSIP) HangupCall(context.Context) error { s.hangups++; return nil }
func (s *fakeSourceSIP) RemoteDialogEnded()               { s.remoteEndeds++ }
```

```go
func (*blockingInviteSIP) AdoptRingingCall() (bool, error)  { return false, nil }
func (*blockingInviteSIP) HangupCall(context.Context) error { return nil }
func (*blockingInviteSIP) RemoteDialogEnded()               {}
```

Add the inbound-dialog fake next to `recordingOutgoingDialog`:

```go
// recordingIncomingDialog is a ringing doorbell call as the manager sees it: it
// records every final or provisional response and every BYE.
type recordingIncomingDialog struct {
	id core.DialogID

	mu        sync.Mutex
	responses []int
	byes      int
}

func (d *recordingIncomingDialog) ID() core.DialogID { return d.id }

func (d *recordingIncomingDialog) Respond(_ context.Context, status int, _, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.responses = append(d.responses, status)

	return nil
}

func (d *recordingIncomingDialog) Bye(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.byes++

	return nil
}

func (d *recordingIncomingDialog) statuses() []int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]int(nil), d.responses...)
}

func (d *recordingIncomingDialog) byeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.byes
}
```

- [ ] **Step 2: Type-check to see them fail**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/`
Expected: errors — `session.PrepareRingingPreview undefined`, `session.Answered undefined`, `session.Ringing undefined`. (The interface itself changes in Step 3; until then the fakes simply have extra methods.)

- [ ] **Step 3: Implement** in `internal/media/source_session.go`.

Replace the `SourceSIP` interface:

```go
type SourceSIP interface {
	// StartStream dials the intercom and reports whether it created the SIP
	// dialog. It reports false when an answered inbound call is already up: no
	// INVITE goes out and the dialog belongs to that call, so this session must
	// never send a BYE for it as a side effect of a media teardown.
	StartStream(context.Context, string) (bool, error)

	// AdoptRingingCall is StartStream for a ringing preview: it never dials, and
	// reports whether the inbound call has already been answered.
	AdoptRingingCall() (bool, error)

	// HangupCall ends an active dialog with a BYE and leaves a ringing call
	// alone. It is the only hangup a media session may reach: rejecting a ringing
	// call with a 603 would end every fork, the indoor unit's included. See
	// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
	HangupCall(context.Context) error

	// RemoteDialogEnded drops the dialog the peer has already torn down,
	// without sending a BYE back. The session calls it on exactly the paths
	// where it skips HangupCall, so the SIP layer is never left holding a dialog
	// nothing will ever end. See SourceSession.RemoteDialogEnded.
	RemoteDialogEnded()
}
```

Add two fields to `SourceSession` after `endCallOnClose`:

```go
	// ringingPreview makes Start borrow the ringing inbound call instead of
	// dialling, and start video only. Set by PrepareRingingPreview before Start.
	ringingPreview bool

	// ringing is true while a started preview is still video-only, waiting for
	// the answer. Answered clears it once audio is flowing.
	ringing bool
```

In `Start`, in the reset block under `s.endCallOnClose = false`, add `s.ringing = false` and capture the mode:

```go
	s.endCallOnClose = false
	s.ringing = false
	preview := s.ringingPreview
```

Replace the SIP step (from `owns, err := s.sip.StartStream(...)` through `s.mu.Unlock()` after `s.ownsDialog = owns`) with:

```go
	// A ringing preview borrows the call: no INVITE, no dialog of its own, so no
	// teardown of this session can end the call while it rings.
	var owns bool

	answered := true

	if preview {
		var err error

		answered, err = s.sip.AdoptRingingCall()
		if err != nil {
			return fmt.Errorf("adopt ringing call: %w", err)
		}
	} else {
		var err error

		owns, err = s.sip.StartStream(startCtx, s.sourceConfig.DevAddr)
		if err != nil {
			return fmt.Errorf("start outgoing sip: %w", err)
		}
	}

	sipStarted = true

	s.mu.Lock()
	s.ownsDialog = owns
	s.mu.Unlock()
```

Replace the AV step (`if err := s.av.Start(...)` block) with:

```go
	ports := AVPorts{Video: videoPort, Audio: audioPort}

	// The intercom has no audio before the answer, so a preview of a call that
	// is still ringing asks for video alone; Answered adds the audio later on
	// the port bound here.
	var avErr error
	if answered {
		avErr = s.av.Start(startCtx, s.sourceConfig.HighResVideo, ports, s.video, s.audio)
	} else {
		avErr = s.av.StartVideo(startCtx, s.sourceConfig.HighResVideo, ports.Video, s.video)
	}

	if avErr != nil {
		return fmt.Errorf("start av: %w", avErr)
	}
```

Immediately before `started = true`, record the phase:

```go
	s.mu.Lock()
	s.ringing = !answered
	s.mu.Unlock()
```

Change the log line `s.logger.InfoContext(startCtx, "source session started")` to:

```go
	s.logger.InfoContext(startCtx, "source session started", "ringing_preview", preview, "ringing", !answered)
```

In the deferred cleanup of `Start` and in `Close`, replace both `s.sip.Hangup(` calls with `s.sip.HangupCall(`. Nothing else changes in those blocks.

Add the new methods after `EndCallOnClose`:

```go
// PrepareRingingPreview makes the next Start serve a consumer that took the
// stream while the inbound call rings: it borrows the call instead of dialling
// and asks for video only. See
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
func (s *SourceSession) PrepareRingingPreview() {
	s.mu.Lock()
	s.ringingPreview = true
	s.mu.Unlock()
}

// Ringing reports whether the session is a started preview still waiting for
// the answer: video only, no audio requested yet.
func (s *SourceSession) Ringing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.ringing
}

// Answered completes a ringing preview once the call is answered: it asks the
// intercom for audio on the port bound at Start. Video, sockets and the
// consumer's session are untouched, which is what makes the answer instant.
// It is a no-op on a session that is not ringing, so a repeated call is safe.
// On failure the session stays ringing and the caller decides what to do.
func (s *SourceSession) Answered(ctx context.Context) error {
	s.mu.Lock()
	if !s.started || !s.ringing {
		s.mu.Unlock()

		return nil
	}
	s.mu.Unlock()

	if err := s.av.StartAudio(ctx, s.audio.Metadata().LocalPort, s.audio); err != nil {
		return fmt.Errorf("start av audio: %w", err)
	}

	s.mu.Lock()
	s.ringing = false
	s.mu.Unlock()

	s.logger.InfoContext(ctx, "ringing preview joined the answered call")

	return nil
}
```

Fix the comment on `SourceSession.RemoteDialogEnded` that says "Close will not run it" for `Hangup`: replace `Hangup is what normally makes the SIP layer forget the dialog` with `HangupCall is what normally makes the SIP layer forget the dialog`.

- [ ] **Step 4: Type-check the package and the module**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/ ./internal/signaling/`
Expected: no output (the `var _ SourceSIP = (*signaling.Manager)(nil)` assertion now holds through Task 1's methods).
Run: `GOOS=linux GOARCH=arm GOARM=7 go build ./...`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/media/source_session.go internal/media/source_session_test.go
git commit -m "feat(media): serve a ringing call with a video-only session that joins the answer

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Coordinator — ringing previews, promotion and stop

**Files:**
- Modify: `internal/media/stream_coordinator.go`
- Test: `internal/media/stream_coordinator_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks directly (talks to sources through the new interface).
- Produces:
  - `var ErrRingingCall = errors.New("media: intercom call is ringing")`
  - `var ErrRingingEnded = errors.New("media: ringing call ended")`
  - `type ManagedSourceRingingPreview interface { PrepareRingingPreview(); Answered(context.Context) error }`
  - `func (c *StreamCoordinator) SetRingingCallProbe(probe func() bool)`
  - `func (c *StreamCoordinator) AcquireRingingPreview(lifecycleCtx, startupCtx context.Context, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error)`
  - `func (c *StreamCoordinator) PromoteRinging()` — non-blocking.
  - `func (c *StreamCoordinator) StopRinging()`

- [ ] **Step 1: Write the failing tests** — append to `internal/media/stream_coordinator_test.go` (add `"sync"` to imports):

```go
// ringingPreviewSource is a managed source that can serve a ringing preview.
// startGate, when set, holds Start until closed; started is signalled when
// Start begins. answeredCh receives one value per Answered call.
type ringingPreviewSource struct {
	mu           sync.Mutex
	prepared     int
	answered     int
	closes       int
	endCallCalls int
	answerErr    error
	packet       *rtp.Packet

	startGate  chan struct{}
	started    chan struct{}
	answeredCh chan struct{}
}

func newRingingPreviewSource() *ringingPreviewSource {
	return &ringingPreviewSource{started: make(chan struct{}, 1), answeredCh: make(chan struct{}, 4)}
}

func (s *ringingPreviewSource) Start(context.Context) error {
	s.started <- struct{}{}
	if s.startGate != nil {
		<-s.startGate
	}

	return nil
}

func (s *ringingPreviewSource) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closes++

	return nil
}

func (s *ringingPreviewSource) EndCallOnClose() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.endCallCalls++
}

func (s *ringingPreviewSource) PrepareRingingPreview() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.prepared++
}

func (s *ringingPreviewSource) Answered(context.Context) error {
	s.mu.Lock()
	s.answered++
	err := s.answerErr
	s.mu.Unlock()

	s.answeredCh <- struct{}{}

	return err
}

func (s *ringingPreviewSource) WriteBackchannelRTP(packet *rtp.Packet) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.packet = packet

	return nil
}

func (s *ringingPreviewSource) counts() (prepared, answered, closes, endCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.prepared, s.answered, s.closes, s.endCallCalls
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func ringingCoordinator(source ManagedSource, ringing *bool) *StreamCoordinator {
	c := NewStreamCoordinator(nil, func(config.Entrypoint, SourceEvents) (ManagedSource, func(), error) {
		return source, nil, nil
	})
	c.SetRingingCallProbe(func() bool { return *ringing })

	return c
}

var testEntrypoint = config.Entrypoint{ID: "main", DevAddr: "20"}

func TestStreamCoordinatorRefusesAnOrdinaryLeaseWhileRinging(t *testing.T) {
	ringing := true
	c := ringingCoordinator(&fakeManagedSource{}, &ringing)

	if _, err := c.Acquire(context.Background(), testEntrypoint, SourceEvents{}); !errors.Is(err, ErrRingingCall) {
		t.Fatalf("Acquire() with an idle stream error = %v, want ErrRingingCall", err)
	}

	// The intercom's own AV for the ring marks the stream external; the refusal
	// must not depend on that.
	c.ObserveControlTrack(true)

	if _, err := c.Acquire(context.Background(), testEntrypoint, SourceEvents{}); !errors.Is(err, ErrRingingCall) {
		t.Fatalf("Acquire() with an external stream error = %v, want ErrRingingCall", err)
	}
}

func TestStreamCoordinatorRingingPreviewAdoptsTheExternalStream(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)
	c.ObserveControlTrack(true)

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatalf("AcquireRingingPreview() error = %v", err)
	}

	if prepared, _, _, _ := source.counts(); prepared != 1 {
		t.Fatalf("PrepareRingingPreview calls = %d, want 1", prepared)
	}

	if owner := c.Snapshot().Owner; owner != StreamOwnerCompanion {
		t.Fatalf("owner = %q, want %q", owner, StreamOwnerCompanion)
	}

	c.Release(lease)
}

func TestStreamCoordinatorRingingPreviewNeedsACapableSource(t *testing.T) {
	ringing := true
	c := ringingCoordinator(&fakeManagedSource{}, &ringing)

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{}); !errors.Is(err, ErrStreamBusy) {
		t.Fatalf("AcquireRingingPreview() error = %v, want ErrStreamBusy", err)
	}

	ringing = false

	lease, err := c.Acquire(context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatalf("Acquire() after the refused preview error = %v, want the lease freed", err)
	}

	c.Release(lease)
}

func TestStreamCoordinatorPreviewOutsideTheRingIsAnOrdinaryLease(t *testing.T) {
	ringing := false
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if prepared, _, _, _ := source.counts(); prepared != 0 {
		t.Fatalf("PrepareRingingPreview calls = %d, want 0", prepared)
	}

	c.StopRinging()

	if _, _, closes, _ := source.counts(); closes != 0 {
		t.Fatalf("closes = %d, want 0 — StopRinging must leave an ordinary lease alone", closes)
	}

	c.Release(lease)
}

func TestStreamCoordinatorStopRingingEndsThePreviewWithoutEndingTheCall(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)

	var failed error

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
		Failed: func(err error) { failed = err },
	}); err != nil {
		t.Fatal(err)
	}

	c.StopRinging()

	if _, _, closes, endCalls := source.counts(); closes != 1 || endCalls != 0 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 0", closes, endCalls)
	}

	if !errors.Is(failed, ErrRingingEnded) {
		t.Fatalf("consumer told %v, want ErrRingingEnded", failed)
	}

	if owner := c.Snapshot().Owner; owner != StreamOwnerIdle {
		t.Fatalf("owner = %q, want idle", owner)
	}
}

func TestStreamCoordinatorPromoteRingingAnswersTheSourceOnce(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	ringing = false
	c.PromoteRinging()
	waitSignal(t, source.answeredCh, "Answered")

	c.PromoteRinging()
	c.StopRinging()

	if _, answered, closes, _ := source.counts(); answered != 1 || closes != 0 {
		t.Fatalf("answered=%d closes=%d, want 1 and 0 — a promoted lease is no longer a preview", answered, closes)
	}

	c.Release(lease)
}

func TestStreamCoordinatorPromotionDuringStartupRunsOnceTheSourceIsUp(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	source.startGate = make(chan struct{})
	c := ringingCoordinator(source, &ringing)

	acquired := make(chan error, 1)
	go func() {
		_, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
		acquired <- err
	}()

	waitSignal(t, source.started, "Start")
	c.PromoteRinging()

	if _, answered, _, _ := source.counts(); answered != 0 {
		t.Fatalf("answered = %d before the source is up, want 0", answered)
	}

	close(source.startGate)

	if err := <-acquired; err != nil {
		t.Fatal(err)
	}

	waitSignal(t, source.answeredCh, "Answered after startup")
}

func TestStreamCoordinatorPromotionFailureStopsTheLeaseWithoutEndingTheCall(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	source.answerErr = errors.New("audio nack")
	c := ringingCoordinator(source, &ringing)

	failed := make(chan error, 1)

	if _, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{
		Failed: func(err error) { failed <- err },
	}); err != nil {
		t.Fatal(err)
	}

	c.PromoteRinging()

	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("consumer told nil, want the promotion error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer was never told the promotion failed")
	}

	if _, _, closes, endCalls := source.counts(); closes != 1 || endCalls != 0 {
		t.Fatalf("closes=%d endCalls=%d, want 1 and 0 — the answered call must survive", closes, endCalls)
	}
}

func TestStreamCoordinatorBlocksTheBackchannelWhileRinging(t *testing.T) {
	ringing := true
	source := newRingingPreviewSource()
	c := ringingCoordinator(source, &ringing)

	lease, err := c.AcquireRingingPreview(context.Background(), context.Background(), testEntrypoint, SourceEvents{})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.WriteBackchannelRTP(lease, testRTPPacket(1)); !errors.Is(err, ErrBackchannelUnavailable) {
		t.Fatalf("WriteBackchannelRTP() while ringing error = %v, want ErrBackchannelUnavailable", err)
	}

	ringing = false
	c.PromoteRinging()
	waitSignal(t, source.answeredCh, "Answered")

	if err := c.WriteBackchannelRTP(lease, testRTPPacket(2)); err != nil {
		t.Fatalf("WriteBackchannelRTP() after the answer error = %v", err)
	}

	c.Release(lease)
}
```

- [ ] **Step 2: Type-check to see them fail**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/`
Expected: errors — `c.SetRingingCallProbe undefined`, `c.AcquireRingingPreview undefined`, `ErrRingingCall undefined`.

- [ ] **Step 3: Implement** in `internal/media/stream_coordinator.go`.

Errors:

```go
var (
	ErrStreamBusy             = errors.New("media: intercom stream is busy")
	ErrExternalStream         = errors.New("media: intercom stream is active externally")
	ErrBackchannelUnavailable = errors.New("media: backchannel is unavailable")
	ErrRingingCall            = errors.New("media: intercom call is ringing")
	ErrRingingEnded           = errors.New("media: ringing call ended")
)
```

Constants (next to `watchInterval`):

```go
	// promotionTimeout bounds the audio request that turns a ringing preview
	// into the answered call: three AV attempts of up to 2 s reply plus 1.2 s
	// flow wait each, with margin.
	promotionTimeout = 15 * time.Second
```

Stop cause (in the `var` block, after `causeStartupTimedOut`):

```go
	causeRingingEnded = stopCause{reason: "ringing call ended"}
```

Interface (after `ManagedSourceCallEnder`):

```go
// ManagedSourceRingingPreview is the capability a source needs to serve a lease
// taken while the inbound call still rings. PrepareRingingPreview runs before
// Start and makes the source borrow the call and start video only; Answered
// adds audio once the call is answered. See
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
type ManagedSourceRingingPreview interface {
	PrepareRingingPreview()
	Answered(context.Context) error
}
```

Fields in `StreamCoordinator`, after `answeredCallProbe`:

```go
	// ringingCallProbe reports whether an inbound call is ringing and not yet
	// answered. While it is, only a ringing preview may take a lease.
	ringingCallProbe func() bool

	// previewRinging marks the current lease as a ringing preview not yet
	// promoted: backchannel blocked, StopRinging applies. promotePending records
	// a CallAnswered that arrived while that lease was still starting.
	previewRinging bool
	promotePending bool

	// leaseEvents are the current lease's events as wrapped by sourceEvents, and
	// consumerEvents the consumer's own. Promotion failure goes through the
	// first, the end of the ring through the second.
	leaseEvents    SourceEvents
	consumerEvents SourceEvents
```

Probe setter and reader (after `answeredCall`):

```go
// SetRingingCallProbe supplies the predicate that tells the coordinator whether
// an inbound call is ringing. While it is, only AcquireRingingPreview may take a
// lease: RTSP and HomeKit wait for the answer.
func (c *StreamCoordinator) SetRingingCallProbe(probe func() bool) {
	c.mu.Lock()
	c.ringingCallProbe = probe
	c.mu.Unlock()
}

// ringingCall runs the probe with c.mu released, for the reason given on
// answeredCall.
func (c *StreamCoordinator) ringingCall() bool {
	c.mu.Lock()
	probe := c.ringingCallProbe
	c.mu.Unlock()

	return probe != nil && probe()
}
```

Acquire entry points — replace `Acquire`, `AcquireWithStartup` and `acquire` with:

```go
func (c *StreamCoordinator) Acquire(ctx context.Context, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error) {
	return c.acquire(ctx, ctx, entrypoint, events, false)
}

// AcquireWithStartup bounds source initialization without tying the live lease
// to the caller's short-lived startup deadline.
func (c *StreamCoordinator) AcquireWithStartup(lifecycleCtx, startupCtx context.Context, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error) {
	return c.acquire(lifecycleCtx, startupCtx, entrypoint, events, false)
}

// AcquireRingingPreview is AcquireWithStartup for the one consumer that may
// also take the stream while the inbound call rings: a WebRTC viewer, which can
// then answer on the same session. The lease starts video only and is promoted
// by PromoteRinging or stopped by StopRinging. Outside a ring it is an ordinary
// lease.
func (c *StreamCoordinator) AcquireRingingPreview(lifecycleCtx, startupCtx context.Context, entrypoint config.Entrypoint, events SourceEvents) (*StreamLease, error) {
	return c.acquire(lifecycleCtx, startupCtx, entrypoint, events, true)
}

func (c *StreamCoordinator) acquire(lifecycleCtx, startupCtx context.Context, entrypoint config.Entrypoint, events SourceEvents, allowRinging bool) (*StreamLease, error) {
	lease, preview, err := c.reserve(lifecycleCtx, entrypoint, allowRinging)
	if err != nil {
		return nil, err
	}

	wrapped := c.sourceEvents(lease, events)

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

	if preview {
		ringingSource, ok := source.(ManagedSourceRingingPreview)
		if !ok {
			c.finishStop(lease, source, cleanup, causeSourceCreation)
			c.logger.ErrorContext(lifecycleCtx, "media source creation failed", "lease_id", lease.id, "entrypoint_id", entrypoint.ID, "error", "source cannot serve a ringing preview")

			return nil, ErrStreamBusy
		}

		ringingSource.PrepareRingingPreview()
	}

	if !c.attachSource(lease, source, cleanup, wrapped, events) {
		return nil, ErrStreamBusy
	}

	if err := c.startSource(lifecycleCtx, startupCtx, lease, source, entrypoint.ID); err != nil {
		return nil, err
	}

	c.finishStart(lease)
	go c.watch(lifecycleCtx, lease)

	return lease, nil
}
```

Replace `reserve`:

```go
// reserve decides who may take the stream. While the inbound call rings only a
// ringing preview may, whatever the stream owner is: a card that closes and
// reopens its preview mid-ring finds the owner idle, and RTSP must still wait.
// The second result says the lease is a ringing preview.
func (c *StreamCoordinator) reserve(ctx context.Context, entrypoint config.Entrypoint, allowRinging bool) (*StreamLease, bool, error) {
	ringing := c.ringingCall()
	answered := c.answeredCall()

	c.mu.Lock()
	if ringing && !allowRinging {
		c.mu.Unlock()
		c.logger.DebugContext(ctx, "stream lease rejected", "entrypoint_id", entrypoint.ID, "reason", "inbound_call_ringing")

		return nil, false, ErrRingingCall
	}

	adopted := c.snapshot.Owner == StreamOwnerExternal

	if adopted && !answered && !ringing {
		c.mu.Unlock()
		c.logger.DebugContext(ctx, "stream lease rejected", "entrypoint_id", entrypoint.ID, "reason", "external_stream_active")

		return nil, false, ErrExternalStream
	}

	if c.leaseID != 0 || c.factory == nil {
		c.mu.Unlock()
		c.logger.DebugContext(ctx, "stream lease rejected", "entrypoint_id", entrypoint.ID, "reason", "source_unavailable", "owner", c.snapshot.Owner)

		return nil, false, ErrStreamBusy
	}

	c.nextID++
	c.leaseID = c.nextID
	c.controlLeaseID = 0
	c.starting = true
	c.previewRinging = ringing
	c.promotePending = false
	c.snapshot = StreamSnapshot{LeaseID: c.leaseID, Owner: StreamOwnerCompanion, EntrypointID: entrypoint.ID, DevAddr: entrypoint.DevAddr, Health: StreamHealthStarting}
	lease := &StreamLease{id: c.leaseID}
	c.mu.Unlock()
	c.logger.InfoContext(ctx, "stream lease acquired", "lease_id", lease.id, "entrypoint_id", entrypoint.ID, "adopted_external", adopted, "ringing_preview", ringing)

	return lease, ringing, nil
}
```

Replace `attachSource`:

```go
func (c *StreamCoordinator) attachSource(lease *StreamLease, source ManagedSource, cleanup func(), wrapped, consumer SourceEvents) bool {
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
	c.leaseEvents, c.consumerEvents = wrapped, consumer
	c.mu.Unlock()

	return true
}
```

Replace `finishStart`:

```go
func (c *StreamCoordinator) finishStart(lease *StreamLease) {
	c.mu.Lock()

	var (
		snapshot StreamSnapshot
		observer StreamStateObserver
		promote  bool
		pSource  ManagedSourceRingingPreview
		pFailed  func(error)
	)

	if c.leaseID == lease.id {
		c.starting = false
		snapshot = c.snapshot
		observer = c.observer

		// The call was answered while this preview was starting.
		if c.promotePending && c.previewRinging && !c.stopping {
			pSource, pFailed = c.takePromotionLocked()
			promote = true
		}
	}
	c.mu.Unlock()

	if observer != nil {
		observer(snapshot)
	}

	if promote {
		go c.promote(lease, pSource, pFailed)
	}
}
```

In `WriteBackchannelRTP`, extend the refusal so the microphone never reaches the intercom before the answer:

```go
	if c.leaseID != lease.id || c.starting || c.stopping || c.previewRinging {
```

In `finishStop`, inside `if c.leaseID == lease.id {`, reset the preview state:

```go
		c.previewRinging = false
		c.promotePending = false
		c.leaseEvents = SourceEvents{}
		c.consumerEvents = SourceEvents{}
```

Add the promotion and stop methods after `Release`:

```go
// PromoteRinging turns the ringing preview into the answered call: the
// backchannel opens and the source asks the intercom for audio, while video,
// sockets and the consumer's session carry on. It returns at once — the audio
// request can take seconds and the caller is the SIP event drain. A lease still
// starting is promoted as soon as its source is up. A no-op on any other lease.
func (c *StreamCoordinator) PromoteRinging() {
	c.mu.Lock()
	if c.leaseID == 0 || !c.previewRinging || c.stopping {
		c.mu.Unlock()

		return
	}

	if c.starting {
		c.promotePending = true
		c.mu.Unlock()

		return
	}

	lease := &StreamLease{id: c.leaseID}
	source, failed := c.takePromotionLocked()
	c.mu.Unlock()

	go c.promote(lease, source, failed)
}

// takePromotionLocked ends the preview phase and returns what promote needs.
// c.mu must be held.
func (c *StreamCoordinator) takePromotionLocked() (ManagedSourceRingingPreview, func(error)) {
	c.previewRinging = false
	c.promotePending = false

	source, _ := c.source.(ManagedSourceRingingPreview)

	return source, c.leaseEvents.Failed
}

// promote asks the source for audio. A failure stops the lease as a source
// failure, which does not end the call: the consumer sees its session close and
// can open a normal one, which adopts the answered call.
func (c *StreamCoordinator) promote(lease *StreamLease, source ManagedSourceRingingPreview, failed func(error)) {
	if source == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), promotionTimeout)
	defer cancel()

	if err := source.Answered(ctx); err != nil {
		c.logger.Warn("ringing preview could not join the answered call; the call stays up", "lease_id", lease.id, "error", err)

		if failed != nil {
			failed(fmt.Errorf("promote ringing preview: %w", err))
		}

		return
	}

	c.logger.Info("ringing preview joined the answered call", "lease_id", lease.id)
}

// StopRinging ends a ringing preview because the ring is over without this
// companion answering: cancelled, timed out, answered elsewhere or rejected. It
// never ends the call, and tells the consumer so it can close its session. A
// no-op on any other lease.
func (c *StreamCoordinator) StopRinging() {
	c.mu.Lock()
	if c.leaseID == 0 || !c.previewRinging {
		c.mu.Unlock()

		return
	}

	lease := &StreamLease{id: c.leaseID}
	consumerFailed := c.consumerEvents.Failed
	c.mu.Unlock()

	if !c.stop(lease, causeRingingEnded) {
		return
	}

	if consumerFailed != nil {
		consumerFailed(ErrRingingEnded)
	}
}
```

- [ ] **Step 4: Type-check, and confirm the watchdog needs no change**

Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/media/`
Expected: no output.

Read `streamHealth` (`internal/media/stream_coordinator.go`, near the end): a lease whose video flows is `video_only`, never `stalled`; `stalled` requires both tracks requested and silent. A ringing preview requests video only, so the watchdog cannot stop it for the missing audio. No change; nothing to commit for this check.

- [ ] **Step 5: Commit**

```bash
git add internal/media/stream_coordinator.go internal/media/stream_coordinator_test.go
git commit -m "feat(media): lease the stream to a ringing preview and promote it on answer

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Wiring — WebRTC asks for previews, call events drive them

**Files:**
- Modify: `internal/media/webrtc.go:369` (`startOfferSource`)
- Modify: `internal/app/run.go:137` (sink), `:396` (probes), `:616-665` (`bridgeSource`), new `routeRingingPreview`
- Test: `internal/app/run_test.go`

**Interfaces:**
- Consumes: `AcquireRingingPreview`, `SetRingingCallProbe`, `PromoteRinging`, `StopRinging`, `ManagedSourceRingingPreview` (Task 4); `SourceSession.PrepareRingingPreview`, `SourceSession.Answered` (Task 3); `Manager.HasRingingInboundCall` (Task 1).
- Produces: `func routeRingingPreview(coordinator ringingPreviewCoordinator, next func(core.Event)) func(core.Event)`.

- [ ] **Step 1: Write the failing test** — append to `internal/app/run_test.go` (add imports it lacks: `"bticino-go-companion/internal/core"`, `"testing"`):

```go
type recordingRingingCoordinator struct{ calls []string }

func (r *recordingRingingCoordinator) PromoteRinging() { r.calls = append(r.calls, "promote") }
func (r *recordingRingingCoordinator) StopRinging()    { r.calls = append(r.calls, "stop") }

func TestRouteRingingPreviewFollowsTheCall(t *testing.T) {
	coordinator := &recordingRingingCoordinator{}

	var applied []core.Event

	route := routeRingingPreview(coordinator, func(event core.Event) { applied = append(applied, event) })

	events := []core.Event{
		core.IncomingCallStarted{DialogID: "d"},
		core.CallAnswered{DialogID: "d"},
		core.IncomingCallEnded{DialogID: "d", Reason: core.CallEndReasonElsewhere},
		core.CallDeclined{DialogID: "d"},
		core.CallHungUp{DialogID: "d"},
		nil,
	}
	for _, event := range events {
		route(event)
	}

	want := []string{"promote", "stop", "stop", "stop"}
	if len(coordinator.calls) != len(want) {
		t.Fatalf("coordinator calls = %v, want %v", coordinator.calls, want)
	}

	for i := range want {
		if coordinator.calls[i] != want[i] {
			t.Fatalf("coordinator calls = %v, want %v", coordinator.calls, want)
		}
	}

	if len(applied) != len(events) {
		t.Fatalf("applied %d events, want %d — every event still reaches the projector", len(applied), len(events))
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `go test ./internal/app/ -run TestRouteRingingPreviewFollowsTheCall -v`
Expected: build failure — `undefined: routeRingingPreview`. If the package cannot run on Windows, use `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/app/` and expect the same undefined error.

- [ ] **Step 3: Implement.**

`internal/media/webrtc.go`, in `startOfferSource`, change the acquire call:

```go
	lease, err := s.coordinator.AcquireRingingPreview(lifecycleCtx, startupCtx, entrypoint, SourceEvents{
```

(The rest of the call is unchanged. The WebRTC viewer is the card, and the one consumer that can answer on the same session.)

`internal/app/run.go`, after `rtspServer.Coordinator().SetAnsweredCallProbe(calls.HasAnsweredInboundCall)`:

```go
	// While the call rings only a ringing preview may take the stream: RTSP and
	// HomeKit wait for the answer. See
	// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
	rtspServer.Coordinator().SetRingingCallProbe(calls.HasRingingInboundCall)
```

Replace line 137:

```go
	runtime.calls.SetEvents(eventSinkFunc(routeRingingPreview(runtime.rtspServer.Coordinator(), applyEvent)))
```

Add after `eventSinkFunc.Publish`:

```go
// ringingPreviewCoordinator is the part of the stream coordinator the call
// events drive.
type ringingPreviewCoordinator interface {
	PromoteRinging()
	StopRinging()
}

// routeRingingPreview lets the call events move a ringing preview along: the
// answer promotes it in place, every way the ring ends without that answer
// stops it. The companion closes the preview on its own rather than relying on
// the card. Neither call blocks the SIP event drain for long: promotion runs on
// its own goroutine, and stopping a preview only closes local sockets. See
// docs/superpowers/specs/2026-10-15-ringing-preview-design.md.
func routeRingingPreview(coordinator ringingPreviewCoordinator, next func(core.Event)) func(core.Event) {
	return func(event core.Event) {
		next(event)

		switch event.(type) {
		case core.CallAnswered:
			coordinator.PromoteRinging()
		case core.IncomingCallEnded, core.CallDeclined, core.CallHungUp:
			coordinator.StopRinging()
		}
	}
}
```

`bridgeSource`: extend the assertion block and add the forwarders after `EndCallOnClose`:

```go
var (
	_ media.ManagedSourceBackchannel     = (*bridgeSource)(nil)
	_ media.ManagedSourceCallEnder       = (*bridgeSource)(nil)
	_ media.ManagedSourceRingingPreview = (*bridgeSource)(nil)
)
```

```go
// PrepareRingingPreview and Answered forward the ringing-preview capability to
// the session, for the same reason EndCallOnClose does: the coordinator finds it
// by type assertion on the *bridgeSource, and a missing forwarder would make
// every preview fail as a source that cannot serve one.
func (s *bridgeSource) PrepareRingingPreview() {
	s.source.PrepareRingingPreview()
}

func (s *bridgeSource) Answered(ctx context.Context) error {
	return s.source.Answered(ctx)
}
```

- [ ] **Step 4: Run the test and build**

Run: `go test ./internal/app/ -run TestRouteRingingPreviewFollowsTheCall -v` (or the `go vet` fallback of Step 2)
Expected: PASS (or no vet output).
Run: `GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/... && GOOS=linux GOARCH=arm GOARM=7 go build ./...`
Expected: no output.
Run: `go test ./internal/signaling/ ./internal/openwebnet/`
Expected: `ok` for both.

- [ ] **Step 5: Commit**

```bash
git add internal/media/webrtc.go internal/app/run.go internal/app/run_test.go
git commit -m "feat(app): drive the ringing preview from the call events

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Card — start the preview on the ring, close it when the ring ends

**Repo:** `D:\Progetti\BTicinoGO-Integration`, branch `feat/sip-incoming-call`.

**Files:**
- Modify: `custom_components/bticino_companion/www/bticino-go-intercom-card.js`

**Interfaces:**
- Consumes: the companion behaviour from Tasks 1-5 through the unchanged `card_webrtc_offer` / `card_webrtc_close` messages.
- Produces (used by Task 7): `this._preview` (boolean), `this._audioSender` (`RTCRtpSender` of the audio transceiver, set only for a preview), `_start(beforeOffer, { preview })`.

- [ ] **Step 1: Constructor** — after `this._elsewhereLatched = false;` add:

```js
    // A ringing preview is the session opened while the call rings: video only,
    // microphone transceiver negotiated but empty until the answer.
    this._preview = false;
    this._previewTried = false;
    this._audioSender = undefined;
```

- [ ] **Step 2: `set hass`** — replace the method with:

```js
  set hass(hass) {
    this._hass = hass;
    // A ringing preview ends with the ring — cancelled, timed out, answered
    // elsewhere or declined. The companion closes its side on its own; this
    // closes ours. An answer from this card turns the call active and keeps it.
    if (this._preview && (this._pc || this._connecting) && !this._isRinging() && this._callState() !== "active") {
      this._end();
    }
    if (this._pc || this._connecting) {
      this._attachVideo();
      return;
    }
    // One preview attempt per ring: a failed one is not retried until the next
    // call, so a refusal cannot turn into a loop of offers.
    if (!this._isRinging()) {
      this._previewTried = false;
    } else if (!this._previewTried && this.isConnected) {
      this._previewTried = true;
      this._start(undefined, { preview: true });
      return;
    }
    // The companion keeps last_incoming_call_end set until the next call starts,
    // so the notice is latched once per call-end: it stays suppressed while the
    // attribute remains true and re-arms only after it has gone back to false.
    if (this._answeredElsewhere()) {
      if (!this._elsewhereLatched) {
        this._elsewhereLatched = true;
        this._elsewhereUntil = Date.now() + 4000;
        setTimeout(() => { this._elsewhereUntil = 0; this._render(); }, 4000);
      }
    } else {
      this._elsewhereLatched = false;
      this._elsewhereUntil = 0;
    }
    this._render();
    this._attachVideo();
  }
```

- [ ] **Step 3: `_start`** — change the signature and the parts that differ for a preview.

Signature and first lines:

```js
  // beforeOffer, when given, is awaited just before the offer goes out: everything
  // up to the local offer is prepared while it is still pending. preview opens the
  // session while the call rings: no microphone is asked for until the answer.
  async _start(beforeOffer, { preview = false } = {}) {
    if (this._pc || this._connecting) return;
    this._connecting = true;
    this._preview = preview;
    this._error = "";
    this._render();
```

Guard the microphone request — change `if (navigator.mediaDevices?.getUserMedia) {` to:

```js
      if (!preview && navigator.mediaDevices?.getUserMedia) {
```

In `onconnectionstatechange`, a dying preview is not an error the user has to read — replace the failure branch with:

```js
        if (["failed", "closed"].includes(pc.connectionState) && this._pc === pc) {
          this._error = this._preview ? "" : "WebRTC connection was lost.";
          this._cleanup();
        }
```

Replace the audio transceiver block (`if (this._micTrack) { ... } else { ... }`) with:

```js
      if (preview) {
        // Negotiated now, filled at the answer with replaceTrack: answering then
        // needs no new offer and the video never stops.
        this._audioSender = pc.addTransceiver("audio", { direction: "sendrecv" }).sender;
      } else if (this._micTrack) {
        pc.addTrack(this._micTrack, this._localStream);
      } else {
        pc.addTransceiver("audio", { direction: "recvonly" });
      }
```

- [ ] **Step 4: `_cleanup`** — after `this._muted = false;` add:

```js
    this._preview = false;
    this._audioSender = undefined;
```

- [ ] **Step 5: `_render`** — four lines change.

```js
    const canAnswer = this._canAnswer() && (!active || this._preview);
```

```js
    const canEnd = (active && !this._preview) || inCall;
```

The status chain gains the preview right after the "answered elsewhere" case:

```js
    const status = this._elsewhereUntil
      ? ["Answered elsewhere", "idle"]
      : this._preview
      ? ["Ringing", "ringing"]
      : this._connecting
```

(the rest of the chain is unchanged), and the call button stays visible during a preview:

```js
        <button class="call ${(active && !this._preview) || (ringing && !canAnswer) ? "hidden" : ""}" title="${canAnswer ? "Answer" : "Start live view"}"><ha-icon icon="mdi:phone"></ha-icon></button>
```

- [ ] **Step 6: Syntax check**

Run (from `D:\Progetti\BTicinoGO-Integration`): `node --check custom_components/bticino_companion/www/bticino-go-intercom-card.js`
Expected: no output.

- [ ] **Step 7: Commit** (Integration repo)

```bash
git add custom_components/bticino_companion/www/bticino-go-intercom-card.js
git commit -m "feat(card): show the intercom video while the doorbell rings

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Card — answer on the preview session

**Repo:** `D:\Progetti\BTicinoGO-Integration`.

**Files:**
- Modify: `custom_components/bticino_companion/www/bticino-go-intercom-card.js` (`_answer`, new `_answerPreview`)

**Interfaces:**
- Consumes: `this._preview`, `this._audioSender` (Task 6).

- [ ] **Step 1: Route the answer** — at the top of `_answer()`, before `this._error = "";`:

```js
    if (this._preview && this._pc) return this._answerPreview();
```

and update the comment inside `_answer` that says an offer sent while ringing "would dial the intercom and be refused busy" to:

```js
    // Without a preview session the offer waits for the answer, so the
    // companion adopts the answered call; the microphone, the ICE configuration
    // and the local offer do not wait, so they are prepared while it is in flight.
    // A failed answer surfaces through _start, which also drops what it prepared.
```

- [ ] **Step 2: Add `_answerPreview`** after `_answer`:

```js
  // The preview session is already up: answering only tells the companion and
  // attaches the microphone to the transceiver negotiated at the start. The
  // microphone is requested at the click, alongside the answer, so the browser
  // still sees the user gesture and neither waits for the other.
  async _answerPreview() {
    const pc = this._pc;
    this._error = "";
    const microphone = navigator.mediaDevices?.getUserMedia
      ? navigator.mediaDevices.getUserMedia({ audio: true }).catch(() => undefined)
      : Promise.resolve(undefined);
    try {
      await this._send("bticino_companion/card_call_answer", {});
    } catch (err) {
      (await microphone)?.getTracks().forEach((track) => track.stop());
      this._error = err?.message || String(err);
      this._render();
      return;
    }
    const stream = await microphone;
    if (this._pc !== pc) {
      stream?.getTracks().forEach((track) => track.stop());
      return;
    }
    this._preview = false;
    if (stream && this._audioSender) {
      this._localStream = stream;
      this._micTrack = stream.getAudioTracks()[0];
      this._micTrack.enabled = !this._muted;
      try {
        await this._audioSender.replaceTrack(this._micTrack);
      } catch (err) {
        console.warn("bticino-go-intercom-card: could not attach the microphone", err);
      }
    }
    this._render();
  }
```

- [ ] **Step 3: Syntax check**

Run: `node --check custom_components/bticino_companion/www/bticino-go-intercom-card.js`
Expected: no output.

- [ ] **Step 4: Commit** (Integration repo)

```bash
git add custom_components/bticino_companion/www/bticino-go-intercom-card.js
git commit -m "feat(card): answer on the preview session without a new offer

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: On-device verification (spec section 7)

Not code: a human runs it on the intercom. Each check is one ring.

**Deploy:**
- Companion: build `dist/companion` with the command in `docs/build-and-install.md` (section "build"), then the "update an installed companion" `scp` + restart command from the same file. This rebuilds `dist/companion`; it is still not committed.
- Card: copy the integration to Home Assistant as usual and hard-reload the dashboard (the card JS is cached).
- On the intercom, keep a SIP capture during the checks: `tcpdump -i lo -s0 -n 'port 5070 or port 5060' -w /tmp/ring.pcap &`, and read it with `tcpdump -r /tmp/ring.pcap -A -n | grep -E '^(INVITE|SIP/2.0|ACK|BYE|CANCEL)'`.

- [ ] **Check 1 — go/no-go.** Card open, ring. Expected: video in the card within about 1 s, status "Ringing", Answer button visible; **the indoor unit keeps ringing and showing its video normally**; `/tmp/companion.log` shows `stream lease acquired ... ringing_preview=true` and `av stream flowing stream=video`. If the indoor unit is disturbed, stop here and report.
- [ ] **Check 2 — ring times out.** Card open, ring, nobody answers. Expected: the card returns to idle when the ring ends; the log shows `stream teardown started ... reason="ringing call ended"`; the capture shows our `480` at 60 s and **no `603`**.
- [ ] **Check 3 — card closed mid-ring.** Ring, open the card, navigate away while it rings. Expected: the indoor unit keeps ringing; no `603`, no `BYE` from us in the capture.
- [ ] **Check 4 — answer from the card.** Ring, wait for the video, press Answer. Expected: the video does not stop; two-way audio within about 1 s; the log shows `ringing preview joined the answered call`. If the visitor does not hear the user, the browser's uplink audio is not reaching the companion (pion may not route the late `replaceTrack` RTP): report it — the fallback is to ask for the microphone at preview start with the track disabled, as the non-preview path already does.
- [ ] **Check 5 — answered on the indoor unit.** Ring, wait for the video, answer on the indoor unit. Expected: the card closes ("Answered elsewhere"), the indoor conversation is normal.

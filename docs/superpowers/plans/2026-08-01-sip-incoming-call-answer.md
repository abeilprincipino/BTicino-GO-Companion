# Incoming SIP Call Answer — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the Home Assistant card able to answer an incoming intercom call, by giving the companion a real inbound SIP path and exposing answer / hang-up controls end to end.

**Architecture:** The companion becomes a Flexisip extension that receives the forked INVITE. A single shared `signaling.Manager` owns both the inbound and the outbound dialog and is the sole source of truth for call state. `POST /api/v3/call/answer` sends only the SIP `200 OK`; media then starts through the *existing* WebRTC path, because `Manager.StartStream` becomes a no-op while a call is answered. No new media code.

**Tech Stack:** Go 1.22+ (`emiago/sipgo v1.3.1`, `pion/webrtc`), Python 3.12+ Home Assistant custom component, vanilla-JS custom element, POSIX `sh` installer.

**Spec:** `docs/superpowers/specs/2026-08-01-sip-incoming-call-answer-design.md`

## Global Constraints

- **Two repos.** Companion: `D:/Progetti/BTicinoGO`, branch `feat/sip-incoming-call` off `main`. Integration: `D:/Progetti/BTicinoGO-Integration`, branch `feat/sip-incoming-call` off `v3` (**not** `main`, **not** `dev/card`).
- **Go verification ladder (Windows host, Linux target).** The companion is Linux-only:
  `internal/media` uses `syscall.Setpgid` and `syscall.Kill`, so `media`, `api`, `openwebnet`,
  `app`, `homekit` and `diagnostics` cannot be compiled or run natively on Windows, and
  `config`/`auth`/`logging`/`storage` have tests that assume POSIX file modes. Every Go task must
  therefore run, in this order, from the companion repo root:

  1. `GOOS=linux GOARCH=arm go build ./...` — production code compiles for the device. Must be silent.
  2. `GOOS=linux GOARCH=arm go vet ./...` — type-checks **including `_test.go` files**. Must be silent.
  3. `go test ./internal/core/ ./internal/signaling/ ./internal/discovery/` — the packages that run
     natively. Must be `ok`.
  4. For a package that cannot run natively, prove the test binary compiles:
     `GOOS=linux GOARCH=arm go test -c -o /dev/null ./internal/<pkg>/`

  Each task below names which of these apply. Where a task's plan text says "Expected: PASS" for a
  package in the non-runnable set, that means steps 1, 2 and 4 succeed — the assertions themselves
  are verified on-device or in CI, not locally. **Pre-existing failures** in
  `config`/`auth`/`logging`/`storage` on Windows are not caused by this work; run new tests in those
  packages with a `-run` filter.
- Python tests: `python -m pytest tests/ -v` from the integration repo root. Tests must **not** import `homeassistant`.
- SIP library is `github.com/emiago/sipgo v1.3.1`. `DialogServerSession.Respond` takes `(statusCode int, reason string, body []byte, headers ...sip.Header)`; `RespondSDP(sdp []byte)` always answers 200 and builds correct headers, including Record-Route.
- Default behaviour must not change for existing installations: `companion.sip.inbound` defaults to `false`, and every other SIP default reproduces today's hardcoded values (`companion@127.0.0.1:5070`, transport `tcp`).
- Go code style in this repo: blank line before `return`, errors wrapped with `fmt.Errorf("...: %w", err)`, structured logging via `slog` with `component` attributes.
- The card must never call the companion directly and must never hold a bearer token. All card traffic goes through Home Assistant WebSocket commands.
- Do not add answer-side `Decline` to the HTTP surface. `Manager.Decline` stays in the package, unexposed.

---

### Task 0: Branches

**Files:** none

- [ ] **Step 1: Create the companion branch**

```bash
cd /d/Progetti/BTicinoGO
git checkout -b feat/sip-incoming-call
```

- [ ] **Step 2: Create the integration branch off v3**

```bash
cd /d/Progetti/BTicinoGO-Integration
git checkout -b feat/sip-incoming-call refs/heads/v3
```

- [ ] **Step 3: Verify both branches**

Run: `git -C /d/Progetti/BTicinoGO branch --show-current && git -C /d/Progetti/BTicinoGO-Integration branch --show-current`
Expected: `feat/sip-incoming-call` printed twice.

---

## Phase A — Companion

### Task 1: SIP configuration section

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.SIP` struct with fields `From, AuthUser, AuthPass, Listen, Transport, Domain string` and `Inbound bool`; reachable as `cfg.Companion.SIP`.

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestDefaultSIPSettings(t *testing.T) {
	t.Parallel()

	cfg, err := Default(Metadata{Model: "C300X", MAC: "aa:bb:cc:dd:ee:ff"})
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}

	sip := cfg.Companion.SIP
	if sip.From != "companion@127.0.0.1:5070" {
		t.Fatalf("From = %q, want companion@127.0.0.1:5070", sip.From)
	}

	if sip.Listen != "127.0.0.1:5070" {
		t.Fatalf("Listen = %q, want 127.0.0.1:5070", sip.Listen)
	}

	if sip.Transport != "tcp" {
		t.Fatalf("Transport = %q, want tcp", sip.Transport)
	}

	if sip.Inbound {
		t.Fatal("Inbound = true, want false by default")
	}
}

func TestSIPSettingsRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")

	cfg, err := Create(path, Metadata{Model: "C300X", MAC: "aa:bb:cc:dd:ee:ff"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	cfg.Companion.SIP.Inbound = true
	cfg.Companion.SIP.From = "companion@127.0.0.1:5075"

	store := &Store{path: path, cfg: cfg}
	if err := store.Save(cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !loaded.Companion.SIP.Inbound {
		t.Fatal("Inbound was not persisted")
	}

	if loaded.Companion.SIP.From != "companion@127.0.0.1:5075" {
		t.Fatalf("From = %q, want companion@127.0.0.1:5075", loaded.Companion.SIP.From)
	}
}
```

If `Store.Save` does not exist with this signature, read `internal/config/config.go` and use the repo's actual persistence entry point; the assertion set stays the same.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run 'TestDefaultSIPSettings|TestSIPSettingsRoundTrip' -v`
Expected: FAIL — `cfg.Companion.SIP undefined`.

- [ ] **Step 3: Add the struct and field**

In `internal/config/config.go`, add after the `Capabilities` type:

```go
// SIP holds the companion's SIP identity. Defaults reproduce the values that
// were hardcoded before inbound calls were supported, so existing
// installations behave identically until Inbound is enabled.
type SIP struct {
	From      string `yaml:"from"`
	AuthUser  string `yaml:"auth_user"`
	AuthPass  string `yaml:"auth_pass"`
	Listen    string `yaml:"listen"`
	Transport string `yaml:"transport"`
	Domain    string `yaml:"domain"`
	Inbound   bool   `yaml:"inbound"`
}
```

Add the field to `Companion`:

```go
type Companion struct {
	DeviceID    string       `yaml:"-"`
	Model       string       `yaml:"-"`
	Entrypoints []Entrypoint `yaml:"entrypoints"`
	SIP         SIP          `yaml:"sip"`
}
```

Add it to `persistedCompanion`:

```go
type persistedCompanion struct {
	Entrypoints []Entrypoint `yaml:"entrypoints"`
	SIP         SIP          `yaml:"sip"`
}
```

- [ ] **Step 4: Add the defaults**

In `Default`, inside the `Companion` literal, after `Entrypoints`:

```go
			SIP: SIP{
				From:      "companion@127.0.0.1:5070",
				AuthUser:  "",
				AuthPass:  "",
				Listen:    "127.0.0.1:5070",
				Transport: "tcp",
				Domain:    "",
				Inbound:   false,
			},
```

- [ ] **Step 5: Wire persistence**

Find where `persistedCompanion` is built from `Config` and where it is read back (the `Load` and save paths in `internal/config/config.go`). Copy `SIP` in both directions alongside `Entrypoints`. Where a loaded config has an empty `SIP.From`, apply the same defaults as `Default` so upgraded installations get sane values rather than empty strings:

```go
func applySIPDefaults(sip *SIP) {
	if strings.TrimSpace(sip.From) == "" {
		sip.From = "companion@127.0.0.1:5070"
	}

	if strings.TrimSpace(sip.Listen) == "" {
		sip.Listen = "127.0.0.1:5070"
	}

	if strings.TrimSpace(sip.Transport) == "" {
		sip.Transport = "tcp"
	}
}
```

Call `applySIPDefaults(&cfg.Companion.SIP)` at the end of `Load`.

- [ ] **Step 6: Run tests**

Run: `go test ./internal/config/ -v`
Expected: PASS, including the two new tests and all pre-existing ones.

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add companion SIP settings section"
```

---

### Task 2: Answer SDP carries DEVADDR

**Files:**
- Modify: `internal/signaling/sdp.go:28-33`
- Modify: `internal/signaling/manager.go` (single call site)
- Test: `internal/signaling/sdp_test.go`

**Interfaces:**
- Produces: `BuildAnswer(host, devAddr string) string`. The `devAddr` line is emitted as `a=DEVADDR:<devAddr>` immediately before the media sections, matching `BuildOffer`.

- [ ] **Step 1: Write the failing test**

Append to `internal/signaling/sdp_test.go`:

```go
func TestBuildAnswerIncludesDevAddr(t *testing.T) {
	t.Parallel()

	answer := BuildAnswer("192.0.2.10", "21")

	if !strings.Contains(answer, "a=DEVADDR:21") {
		t.Fatalf("answer missing DEVADDR: %s", answer)
	}

	devAddrIndex := strings.Index(answer, "a=DEVADDR:21")
	audioIndex := strings.Index(answer, "m=audio")

	if devAddrIndex < 0 || audioIndex < 0 || devAddrIndex > audioIndex {
		t.Fatalf("DEVADDR must precede the media sections: %s", answer)
	}
}

func TestBuildAnswerOmitsEmptyDevAddr(t *testing.T) {
	t.Parallel()

	answer := BuildAnswer("192.0.2.10", "")

	if strings.Contains(answer, "DEVADDR") {
		t.Fatalf("answer must not contain DEVADDR when devaddr is empty: %s", answer)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/signaling/ -run TestBuildAnswer -v`
Expected: FAIL — `too many arguments in call to BuildAnswer`.

- [ ] **Step 3: Change the implementation**

Replace `BuildAnswer` in `internal/signaling/sdp.go`:

```go
func BuildAnswer(host, devAddr string) string {
	lines := sessionLines(host, "3747", "461")

	devAddr = strings.TrimSpace(devAddr)
	if devAddr != "" {
		lines = append(lines, "a=DEVADDR:"+devAddr)
	}

	lines = append(lines, mediaLines()...)

	return strings.Join(lines, "\r\n") + "\r\n"
}
```

- [ ] **Step 4: Update the call site**

In `internal/signaling/manager.go`, inside `Answer`, change:

```go
	if err := m.incoming.Respond(ctx, 200, "OK", BuildAnswer(m.host)); err != nil {
```

to:

```go
	if err := m.incoming.Respond(ctx, 200, "OK", BuildAnswer(m.host, "")); err != nil {
```

The empty devaddr is intentional for this task: the Manager has nowhere to obtain one yet. Task 4
introduces `m.incomingDevAddr` and replaces the `""`. Do **not** add that field now — an unused
struct field is dead code.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/signaling/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/signaling/sdp.go internal/signaling/sdp_test.go internal/signaling/manager.go
git commit -m "feat(signaling): include DEVADDR in the SIP answer SDP"
```

---

### Task 3: Call-end reasons in the core state

**Files:**
- Modify: `internal/core/events.go:55-60`
- Modify: `internal/core/state.go`
- Test: `internal/core/state_test.go`

**Interfaces:**
- Produces: `core.CallEndReason` (`CallEndReasonCancelled`, `CallEndReasonTimeout`, `CallEndReasonElsewhere`); `core.IncomingCallEnded{DialogID, Reason}`; `core.State.LastIncomingCallEnd *IncomingCallEnd` serialised as `last_incoming_call_end` with fields `dialog_id` and `reason`.

- [ ] **Step 1: Write the failing test**

Append to `internal/core/state_test.go`:

```go
func TestIncomingCallEndedRecordsReason(t *testing.T) {
	t.Parallel()

	projector := NewProjector()

	if _, err := projector.Apply(IncomingCallStarted{DialogID: "d1", EntrypointID: "main"}); err != nil {
		t.Fatalf("IncomingCallStarted error = %v", err)
	}

	if _, err := projector.Apply(IncomingCallEnded{DialogID: "d1", Reason: CallEndReasonElsewhere}); err != nil {
		t.Fatalf("IncomingCallEnded error = %v", err)
	}

	state := projector.Snapshot()
	if state.IncomingCall != nil {
		t.Fatal("IncomingCall must be cleared")
	}

	if state.LastIncomingCallEnd == nil {
		t.Fatal("LastIncomingCallEnd must be recorded")
	}

	if state.LastIncomingCallEnd.Reason != CallEndReasonElsewhere {
		t.Fatalf("Reason = %q, want elsewhere", state.LastIncomingCallEnd.Reason)
	}

	if state.CallState != CallStateIdle {
		t.Fatalf("CallState = %q, want idle", state.CallState)
	}
}

func TestIncomingCallStartedClearsPreviousEndReason(t *testing.T) {
	t.Parallel()

	projector := NewProjector()

	if _, err := projector.Apply(IncomingCallStarted{DialogID: "d1", EntrypointID: "main"}); err != nil {
		t.Fatal(err)
	}

	if _, err := projector.Apply(IncomingCallEnded{DialogID: "d1", Reason: CallEndReasonElsewhere}); err != nil {
		t.Fatal(err)
	}

	if _, err := projector.Apply(IncomingCallStarted{DialogID: "d2", EntrypointID: "main"}); err != nil {
		t.Fatal(err)
	}

	if projector.Snapshot().LastIncomingCallEnd != nil {
		t.Fatal("a new incoming call must clear the previous end reason")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/core/ -run TestIncomingCall -v`
Expected: FAIL — `unknown field Reason in struct literal`.

- [ ] **Step 3: Extend the event**

In `internal/core/events.go`, replace the `IncomingCallEnded` block:

```go
// CallEndReason explains why a pending incoming call stopped without being
// answered on this endpoint.
type CallEndReason string

const (
	CallEndReasonCancelled CallEndReason = "cancelled"
	CallEndReasonTimeout   CallEndReason = "timeout"
	CallEndReasonElsewhere CallEndReason = "elsewhere"
)

type IncomingCallEnded struct {
	DialogID DialogID
	Reason   CallEndReason
}

func (IncomingCallEnded) Type() EventType { return EventIncomingCallEnded }
func (IncomingCallEnded) event()          {}
```

- [ ] **Step 4: Extend the state**

In `internal/core/state.go`, add the type next to `IncomingCall`:

```go
type IncomingCallEnd struct {
	DialogID DialogID      `json:"dialog_id"`
	Reason   CallEndReason `json:"reason"`
}
```

Add the field to `State`, after `IncomingCall`:

```go
	LastIncomingCallEnd *IncomingCallEnd `json:"last_incoming_call_end,omitempty"`
```

In `apply`, replace the `IncomingCallEnded` case:

```go
	case IncomingCallEnded:
		if state.IncomingCall == nil {
			return nil
		}

		if event.DialogID != state.IncomingCall.DialogID {
			return transitionError("incoming dialog %q does not exist", event.DialogID)
		}

		state.IncomingCall = nil
		state.LastIncomingCallEnd = &IncomingCallEnd{DialogID: event.DialogID, Reason: event.Reason}
```

In the `IncomingCallStarted` case, clear the previous reason right after the guard:

```go
		state.LastIncomingCallEnd = nil
		state.IncomingCall = &IncomingCall{DialogID: event.DialogID, EntrypointID: event.EntrypointID}
```

In the `CallAnswered` case, add the same clear before assigning `ActiveCall`:

```go
		state.LastIncomingCallEnd = nil
```

- [ ] **Step 5: Extend cloneState**

In `cloneState`, after the `IncomingCall` block:

```go
	if state.LastIncomingCallEnd != nil {
		lastEnd := *state.LastIncomingCallEnd
		stateCopy.LastIncomingCallEnd = &lastEnd
	}
```

- [ ] **Step 6: Run tests**

Run: `go test ./internal/core/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/core/events.go internal/core/state.go internal/core/state_test.go
git commit -m "feat(core): record why an incoming call ended"
```

---

### Task 4: Manager owns the answered dialog

**Files:**
- Modify: `internal/signaling/manager.go`
- Test: `internal/signaling/manager_test.go`

**Interfaces:**
- Produces:
  - `type EntrypointResolver func() (core.EntrypointID, string)` — returns the entrypoint ID and its devaddr for an inbound call; empty ID means "cannot attribute".
  - `NewManager(host string, dialer StreamDialer, events EventSink, resolve EntrypointResolver) *Manager`
  - `(*Manager).SetEvents(EventSink)`
  - `(*Manager).OnInvite(ctx context.Context, dialog IncomingDialog) error` — **signature changed**, no longer takes an entrypoint ID
  - `(*Manager).EndIncoming(reason core.CallEndReason)`
  - `(*Manager).Answer(ctx) error`, `(*Manager).Hangup(ctx) error` (idempotent), `(*Manager).StartStream(ctx, devAddr) error`
- Consumes: `core.CallEndReason` and `BuildAnswer(host, devAddr)` from Tasks 2–3.

- [ ] **Step 1: Write the failing tests**

Replace `TestManager_OnInviteStoresDialogRingsAndPublishes`, `TestManager_AnswerMovesIncomingToActiveWithSDP` and `TestManager_StartStreamRejectsIncomingWithoutAnswering` in `internal/signaling/manager_test.go` with the versions below, and add the four new tests. Also add the shared helper.

```go
func testResolver(id core.EntrypointID, devAddr string) EntrypointResolver {
	return func() (core.EntrypointID, string) { return id, devAddr }
}

func TestManager_OnInviteStoresDialogRingsAndPublishes(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &fakeEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatalf("OnInvite() error = %v", err)
	}

	if len(dialog.responses) != 1 || dialog.responses[0].status != 180 || dialog.responses[0].reason != "Ringing" {
		t.Fatalf("responses = %#v, want 180 Ringing", dialog.responses)
	}

	if len(events.events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events.events))
	}

	if event, ok := events.events[0].(core.IncomingCallStarted); !ok || event.DialogID != testDialogID || event.EntrypointID != "main" {
		t.Fatalf("event = %#v, want IncomingCallStarted for dialog-1/main", events.events[0])
	}
}

func TestManager_OnInviteRejectsUnattributableCall(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &fakeEventSink{}
	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("", ""))

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatalf("OnInvite() error = %v", err)
	}

	if len(dialog.responses) != 1 || dialog.responses[0].status != 486 {
		t.Fatalf("responses = %#v, want 486 Busy Here", dialog.responses)
	}

	if len(events.events) != 0 {
		t.Fatalf("events = %#v, want none", events.events)
	}
}

func TestManager_AnswerMovesIncomingToActiveWithSDP(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &fakeEventSink{}

	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatalf("Answer() error = %v", err)
	}

	if len(dialog.responses) != 2 || dialog.responses[1].status != 200 || dialog.responses[1].reason != "OK" {
		t.Fatalf("responses = %#v, want trailing 200 OK", dialog.responses)
	}

	if !strings.Contains(dialog.responses[1].body, "a=DEVADDR:21") {
		t.Fatalf("answer SDP missing DEVADDR: %s", dialog.responses[1].body)
	}

	if !strings.Contains(dialog.responses[1].body, "m=audio 65000 RTP/SAVP 110") || !strings.Contains(dialog.responses[1].body, "m=video 65002 RTP/SAVP 96") {
		t.Fatalf("answer SDP has wrong ports: %s", dialog.responses[1].body)
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup() error = %v", err)
	}

	if dialog.byes != 1 {
		t.Fatalf("bye count = %d, want 1", dialog.byes)
	}
}

func TestManager_StartStreamRejectsIncomingWithoutAnswering(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	dialer := &fakeDialer{}

	manager := NewManager("192.0.2.10", dialer, &fakeEventSink{}, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	err := manager.StartStream(context.Background(), "21")
	if !errors.Is(err, ErrIncomingDialog) {
		t.Fatalf("StartStream() error = %v, want ErrIncomingDialog", err)
	}

	if dialer.calls != 0 {
		t.Fatalf("dialer calls = %d, want 0", dialer.calls)
	}
}

func TestManager_StartStreamSkipsInviteWhileCallAnswered(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	dialer := &fakeDialer{}

	manager := NewManager("192.0.2.10", dialer, &fakeEventSink{}, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := manager.StartStream(context.Background(), "21"); err != nil {
		t.Fatalf("StartStream() error = %v, want nil", err)
	}

	if dialer.calls != 0 {
		t.Fatalf("dialer calls = %d, want 0 — no outbound INVITE while a call is answered", dialer.calls)
	}
}

func TestManager_HangupIsIdempotent(t *testing.T) {
	t.Parallel()

	manager := NewManager("192.0.2.10", &fakeDialer{}, &fakeEventSink{}, testResolver("main", "21"))

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("Hangup() on idle manager error = %v, want nil", err)
	}

	dialog := &fakeIncomingDialog{id: testDialogID}
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	if err := manager.Answer(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("first Hangup() error = %v", err)
	}

	if err := manager.Hangup(context.Background()); err != nil {
		t.Fatalf("second Hangup() error = %v, want nil", err)
	}

	if dialog.byes != 1 {
		t.Fatalf("bye count = %d, want 1", dialog.byes)
	}
}

func TestManager_EndIncomingPublishesReason(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &fakeEventSink{}

	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))
	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	manager.EndIncoming(core.CallEndReasonElsewhere)

	if len(events.events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events.events))
	}

	event, ok := events.events[1].(core.IncomingCallEnded)
	if !ok || event.DialogID != testDialogID || event.Reason != core.CallEndReasonElsewhere {
		t.Fatalf("event = %#v, want IncomingCallEnded/elsewhere", events.events[1])
	}

	manager.EndIncoming(core.CallEndReasonCancelled)

	if len(events.events) != 2 {
		t.Fatalf("EndIncoming must be a no-op when nothing is pending, got %d events", len(events.events))
	}

	if err := manager.Answer(context.Background()); !errors.Is(err, ErrNoIncomingDialog) {
		t.Fatalf("Answer() error = %v, want ErrNoIncomingDialog", err)
	}
}

func TestManager_IncomingCallExpires(t *testing.T) {
	t.Parallel()

	dialog := &fakeIncomingDialog{id: testDialogID}
	events := &fakeEventSink{}

	manager := NewManager("192.0.2.10", &fakeDialer{}, events, testResolver("main", "21"))
	manager.SetIncomingTimeout(20 * time.Millisecond)

	if err := manager.OnInvite(context.Background(), dialog); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := manager.Answer(context.Background()); errors.Is(err, ErrNoIncomingDialog) {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("incoming call did not expire")
		}

		time.Sleep(5 * time.Millisecond)
	}
}
```

Update the existing `TestManager_DeclineSends603AndClearsIncoming`, `TestManager_StartStreamRejectsActiveOutgoingDialog` and any other `NewManager(` / `OnInvite(` call in the file to the new signatures. Add `"time"` to the test file imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/signaling/ -v`
Expected: FAIL — `not enough arguments in call to NewManager`.

- [ ] **Step 3: Rewrite the Manager**

Replace the whole body of `internal/signaling/manager.go` below the imports with:

```go
var (
	ErrNoIncomingDialog = errors.New("sip: no incoming dialog")
	ErrIncomingDialog   = errors.New("sip: an incoming dialog exists")
	ErrActiveDialog     = errors.New("sip: an active dialog exists")
)

const defaultIncomingTimeout = 60 * time.Second

type IncomingDialog interface {
	ID() core.DialogID
	Respond(context.Context, int, string, string) error
	Bye(context.Context) error
}

type OutgoingDialog interface {
	Bye(context.Context) error
}

type StreamDialer interface {
	StartStream(context.Context, string, string) (OutgoingDialog, error)
}

type EventSink interface {
	Publish(core.Event)
}

// EntrypointResolver attributes an inbound call to a configured entrypoint and
// returns its devaddr. An empty ID means the call cannot be attributed.
type EntrypointResolver func() (core.EntrypointID, string)

// Manager owns the single inbound and the single outbound SIP dialog. It is the
// only component that knows whether a real dialog exists.
type Manager struct {
	mu sync.Mutex

	host    string
	dialer  StreamDialer
	events  EventSink
	resolve EntrypointResolver

	incoming        IncomingDialog
	incomingDevAddr string
	incomingExpiry  *time.Timer
	incomingTimeout time.Duration

	active         OutgoingDialog
	activeID       core.DialogID
	activeIncoming bool
}

func NewManager(host string, dialer StreamDialer, events EventSink, resolve EntrypointResolver) *Manager {
	if resolve == nil {
		resolve = func() (core.EntrypointID, string) { return "", "" }
	}

	return &Manager{host: host, dialer: dialer, events: events, resolve: resolve, incomingTimeout: defaultIncomingTimeout}
}

// SetEvents assigns the sink after construction, because the projector-backed
// applier is not available when the manager is created.
func (m *Manager) SetEvents(events EventSink) {
	m.mu.Lock()
	m.events = events
	m.mu.Unlock()
}

// SetIncomingTimeout overrides how long an unanswered inbound call is kept.
func (m *Manager) SetIncomingTimeout(timeout time.Duration) {
	m.mu.Lock()
	m.incomingTimeout = timeout
	m.mu.Unlock()
}

func (m *Manager) OnInvite(ctx context.Context, dialog IncomingDialog) error {
	m.mu.Lock()

	if m.incoming != nil || m.active != nil {
		m.mu.Unlock()
		return dialog.Respond(ctx, 486, "Busy Here", "")
	}

	entrypointID, devAddr := m.resolve()
	if entrypointID == "" {
		m.mu.Unlock()
		return dialog.Respond(ctx, 486, "Busy Here", "")
	}

	m.mu.Unlock()

	if err := dialog.Respond(ctx, 180, "Ringing", ""); err != nil {
		return err
	}

	m.mu.Lock()
	m.incoming = dialog
	m.incomingDevAddr = devAddr
	m.startIncomingExpiryLocked(dialog)
	m.mu.Unlock()

	m.publish(core.IncomingCallStarted{DialogID: dialog.ID(), EntrypointID: entrypointID})

	return nil
}

func (m *Manager) Answer(ctx context.Context) error {
	m.mu.Lock()

	dialog, devAddr := m.incoming, m.incomingDevAddr
	if dialog == nil {
		m.mu.Unlock()
		return ErrNoIncomingDialog
	}

	m.mu.Unlock()

	if err := dialog.Respond(ctx, 200, "OK", BuildAnswer(m.host, devAddr)); err != nil {
		return err
	}

	m.mu.Lock()

	if m.incoming != dialog {
		m.mu.Unlock()
		return ErrNoIncomingDialog
	}

	m.stopIncomingExpiryLocked()
	m.incoming = nil
	m.incomingDevAddr = ""
	m.active = dialog
	m.activeID = dialog.ID()
	m.activeIncoming = true
	m.mu.Unlock()

	m.publish(core.CallAnswered{DialogID: dialog.ID()})

	return nil
}

// Decline is retained for the concurrent-call path and is deliberately not
// exposed over HTTP.
func (m *Manager) Decline(ctx context.Context) error {
	m.mu.Lock()

	dialog := m.incoming
	if dialog == nil {
		m.mu.Unlock()
		return ErrNoIncomingDialog
	}

	m.stopIncomingExpiryLocked()
	m.incoming = nil
	m.incomingDevAddr = ""
	m.mu.Unlock()

	if err := dialog.Respond(ctx, 603, "Decline", ""); err != nil {
		return err
	}

	m.publish(core.CallDeclined{DialogID: dialog.ID()})

	return nil
}

// Hangup is idempotent: tearing down a call that is already gone is not an
// error, because SourceSession.Close runs it again on every normal teardown.
func (m *Manager) Hangup(ctx context.Context) error {
	m.mu.Lock()

	if active := m.active; active != nil {
		dialogID := m.activeID
		m.active = nil
		m.activeID = ""
		m.activeIncoming = false
		m.mu.Unlock()

		err := active.Bye(ctx)
		if dialogID != "" {
			m.publish(core.CallHungUp{DialogID: dialogID})
		}

		if err != nil {
			return fmt.Errorf("sip bye: %w", err)
		}

		return nil
	}

	dialog := m.incoming
	if dialog == nil {
		m.mu.Unlock()
		return nil
	}

	m.stopIncomingExpiryLocked()
	m.incoming = nil
	m.incomingDevAddr = ""
	m.mu.Unlock()

	if err := dialog.Respond(ctx, 603, "Decline", ""); err != nil {
		return err
	}

	m.publish(core.CallHungUp{DialogID: dialog.ID()})

	return nil
}

// EndIncoming clears a pending inbound call that will never be answered here.
func (m *Manager) EndIncoming(reason core.CallEndReason) {
	m.clearIncoming(nil, reason)
}

func (m *Manager) clearIncoming(expected IncomingDialog, reason core.CallEndReason) bool {
	m.mu.Lock()

	dialog := m.incoming
	if dialog == nil || (expected != nil && dialog != expected) {
		m.mu.Unlock()
		return false
	}

	m.stopIncomingExpiryLocked()
	m.incoming = nil
	m.incomingDevAddr = ""
	m.mu.Unlock()

	m.publish(core.IncomingCallEnded{DialogID: dialog.ID(), Reason: reason})

	return true
}

func (m *Manager) startIncomingExpiryLocked(dialog IncomingDialog) {
	m.stopIncomingExpiryLocked()

	timeout := m.incomingTimeout
	if timeout <= 0 {
		timeout = defaultIncomingTimeout
	}

	m.incomingExpiry = time.AfterFunc(timeout, func() {
		if !m.clearIncoming(dialog, core.CallEndReasonTimeout) {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = dialog.Respond(ctx, 480, "Temporarily Unavailable", "")
	})
}

func (m *Manager) stopIncomingExpiryLocked() {
	if m.incomingExpiry != nil {
		m.incomingExpiry.Stop()
		m.incomingExpiry = nil
	}
}

func (m *Manager) StartStream(ctx context.Context, devAddr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.incoming != nil {
		return ErrIncomingDialog
	}

	// The intercom is already streaming for the answered call; a second INVITE
	// would come back as 486 Busy Here.
	if m.activeIncoming {
		return nil
	}

	if m.active != nil {
		return ErrActiveDialog
	}

	dialog, err := m.dialer.StartStream(ctx, devAddr, BuildOffer(m.host, devAddr))
	if err != nil {
		return err
	}

	m.active = dialog

	return nil
}

func (m *Manager) publish(event core.Event) {
	m.mu.Lock()
	events := m.events
	m.mu.Unlock()

	if events != nil {
		events.Publish(event)
	}
}
```

Set the import block to:

```go
import (
	"bticino-go-companion/internal/core"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)
```

`ErrNoActiveDialog` is deleted: `Hangup` is idempotent, so nothing returns it, and no code outside
`internal/signaling` referenced it. Verify with `grep -rn ErrNoActiveDialog --include=*.go .` before
committing — the result must be empty.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/signaling/ -v`
Expected: PASS for every test in the package.

- [ ] **Step 5: Commit**

```bash
git add internal/signaling/manager.go internal/signaling/manager_test.go
git commit -m "feat(signaling): manager owns answered inbound dialogs"
```

---

### Task 5: Mapper stops inventing dialogs

**Files:**
- Modify: `internal/openwebnet/mapper.go`
- Test: `internal/openwebnet/mapper_test.go` (create if absent; the existing assertions may live in `internal/openwebnet/frames_test.go` — search for `IncomingCallStarted` and update wherever it is)

**Interfaces:**
- Produces: `Mapper.Map` returns `[]core.Event{core.RingStarted{...}}` on ring start and `[]core.Event{core.RingCleared{...}}` on stream stop. It no longer emits `IncomingCallStarted` or `CallHungUp`.

- [ ] **Step 1: Write the failing test**

Add to the mapper's test file:

```go
func TestMapperEmitsOnlyPhysicalRingEvents(t *testing.T) {
	t.Parallel()

	mapper := NewMapper([]config.Entrypoint{{ID: "main", DevAddr: "20"}})

	events := mapper.Map(Message{System: "open", Raw: "*8*1#1#4#20*20##"})
	if len(events) != 1 {
		t.Fatalf("ring start events = %#v, want exactly one", events)
	}

	if _, ok := events[0].(core.RingStarted); !ok {
		t.Fatalf("event = %#v, want RingStarted", events[0])
	}

	stopEvents := mapper.Map(Message{System: "open", Raw: FrameStop})
	if len(stopEvents) != 1 {
		t.Fatalf("stop events = %#v, want exactly one", stopEvents)
	}

	if _, ok := stopEvents[0].(core.RingCleared); !ok {
		t.Fatalf("event = %#v, want RingCleared", stopEvents[0])
	}
}
```

If `*8*1#1#4#20*20##` does not resolve to entrypoint `main` in this repo's frame format, copy the exact ring frame used by the existing mapper tests.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/openwebnet/ -run TestMapperEmitsOnlyPhysicalRingEvents -v`
Expected: FAIL — two events returned, second is `core.IncomingCallStarted`.

- [ ] **Step 3: Simplify the mapper**

In `internal/openwebnet/mapper.go`, delete the `dialog` field from the struct:

```go
type Mapper struct {
	mu               sync.Mutex
	entrypoints      map[string]core.EntrypointID
	recentFrames     map[string]time.Time
	activeEntrypoint core.EntrypointID
}
```

Replace the ring-start branch:

```go
	if IsRingStart(raw) {
		if m.activeEntrypoint != "" {
			return nil
		}

		id := m.resolveEntrypoint(raw)
		if id == "" {
			return nil
		}

		m.activeEntrypoint = id

		return []core.Event{core.RingStarted{EntrypointID: id}}
	}
```

Replace the stop branch:

```go
	if IsStreamStop(raw) || IsFreeAVResources(raw) {
		if m.activeEntrypoint == "" {
			return nil
		}

		events := []core.Event{core.RingCleared{EntrypointID: m.activeEntrypoint}}
		m.activeEntrypoint = ""

		return events
	}
```

Remove the now-unused `fmt` import if nothing else in the file uses it.

- [ ] **Step 4: Fix the other mapper tests**

Run: `go test ./internal/openwebnet/ -v` and update every assertion that expected `IncomingCallStarted` or `CallHungUp` from the mapper. They must now expect only the ring events.

- [ ] **Step 5: Run the full package**

Run: `go test ./internal/openwebnet/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/openwebnet/mapper.go internal/openwebnet/
git commit -m "refactor(openwebnet): mapper reports physical ring only"
```

---

### Task 6: Dialer accepts inbound INVITEs

**Files:**
- Modify: `internal/signaling/dialer.go`
- Test: `internal/signaling/dialer_test.go`

**Interfaces:**
- Consumes: `Manager.OnInvite(ctx, IncomingDialog) error`, `Manager.EndIncoming(core.CallEndReason)`.
- Produces:
  - `StreamDialerConfig` gains `Inbound bool`.
  - `type InboundHandler interface { OnInvite(context.Context, IncomingDialog) error; EndIncoming(core.CallEndReason) }`
  - `(*streamDialer).SetInboundHandler(InboundHandler)`
  - `cancelReason(req *sip.Request) core.CallEndReason` — `CallEndReasonElsewhere` when the CANCEL carries a `Reason` header containing `cause=200`, otherwise `CallEndReasonCancelled`.

- [ ] **Step 1: Write the failing test**

Append to `internal/signaling/dialer_test.go`:

```go
func TestCancelReasonDetectsAnsweredElsewhere(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		want   core.CallEndReason
	}{
		{name: "answered elsewhere", header: `SIP;cause=200;text="Call completed elsewhere"`, want: core.CallEndReasonElsewhere},
		{name: "caller gave up", header: `SIP;cause=487;text="Request Terminated"`, want: core.CallEndReasonCancelled},
		{name: "no header", header: "", want: core.CallEndReasonCancelled},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			req := sip.NewRequest(sip.CANCEL, sip.Uri{Scheme: "sip", User: "companion", Host: "127.0.0.1"})
			if test.header != "" {
				req.AppendHeader(sip.NewHeader("Reason", test.header))
			}

			if got := cancelReason(req); got != test.want {
				t.Fatalf("cancelReason() = %q, want %q", got, test.want)
			}
		})
	}
}
```

Ensure the test file imports `"bticino-go-companion/internal/core"` and `"github.com/emiago/sipgo/sip"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/signaling/ -run TestCancelReason -v`
Expected: FAIL — `undefined: cancelReason`.

- [ ] **Step 3: Add the inbound plumbing to the dialer**

In `internal/signaling/dialer.go`, add `Inbound bool` to `StreamDialerConfig`, and add these fields to `streamDialer`:

```go
	in        *sipgo.DialogServerCache
	inboundMu sync.RWMutex
	inbound   InboundHandler
```

Add the interface and the adapter at the end of the file:

```go
// InboundHandler receives inbound dialog lifecycle events from the SIP server.
type InboundHandler interface {
	OnInvite(context.Context, IncomingDialog) error
	EndIncoming(core.CallEndReason)
}

// incomingDialog adapts a sipgo server session to the signaling interface.
type incomingDialog struct {
	session *sipgo.DialogServerSession
	id      core.DialogID
}

func (d *incomingDialog) ID() core.DialogID { return d.id }

// Respond sends a provisional or final response. A non-empty body always means
// the 200 OK answer, for which RespondSDP builds the correct headers.
func (d *incomingDialog) Respond(_ context.Context, status int, reason, body string) error {
	if body != "" {
		return d.session.RespondSDP([]byte(body))
	}

	return d.session.Respond(status, reason, nil)
}

func (d *incomingDialog) Bye(ctx context.Context) error {
	defer func() { _ = d.session.Close() }()

	return d.session.Bye(ctx)
}

func cancelReason(req *sip.Request) core.CallEndReason {
	header := req.GetHeader("Reason")
	if header != nil && strings.Contains(header.Value(), "cause=200") {
		return core.CallEndReasonElsewhere
	}

	return core.CallEndReasonCancelled
}
```

Add `"bticino-go-companion/internal/core"` to the dialer imports.

- [ ] **Step 4: Register the handlers**

In `NewStreamDialer`, after `server.OnBye(dialer.onBye)`:

```go
	if cfg.Inbound {
		dialer.in = sipgo.NewDialogServerCache(client, contact)
		server.OnInvite(dialer.onInvite)
		server.OnCancel(dialer.onCancel)
		server.OnAck(dialer.onAck)
		dialer.logger.Info("inbound sip enabled")
	}
```

Add the handlers and the setter:

```go
// SetInboundHandler assigns the component that decides how inbound calls are
// answered. Inbound requests are rejected until it is set.
func (d *streamDialer) SetInboundHandler(handler InboundHandler) {
	d.inboundMu.Lock()
	d.inbound = handler
	d.inboundMu.Unlock()
}

func (d *streamDialer) inboundHandler() InboundHandler {
	d.inboundMu.RLock()
	defer d.inboundMu.RUnlock()

	return d.inbound
}

func (d *streamDialer) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	handler := d.inboundHandler()
	if handler == nil || d.in == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
		return
	}

	session, err := d.in.ReadInvite(req, tx)
	if err != nil {
		d.logger.Warn("inbound invite rejected", "error", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Server Error", nil))

		return
	}

	dialogID := core.DialogID("")
	if callID := req.CallID(); callID != nil {
		dialogID = core.DialogID(callID.Value())
	}

	d.logger.Info("inbound sip invite received", "dialog_id", string(dialogID))

	if err := handler.OnInvite(context.Background(), &incomingDialog{session: session, id: dialogID}); err != nil {
		d.logger.Warn("inbound invite handling failed", "dialog_id", string(dialogID), "error", err)
		_ = session.Close()
	}
}

func (d *streamDialer) onCancel(req *sip.Request, tx sip.ServerTransaction) {
	_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))

	reason := cancelReason(req)
	d.logger.Info("inbound sip cancel received", "reason", string(reason))

	if handler := d.inboundHandler(); handler != nil {
		handler.EndIncoming(reason)
	}
}

func (d *streamDialer) onAck(req *sip.Request, tx sip.ServerTransaction) {
	if d.in == nil {
		return
	}

	if err := d.in.ReadAck(req, tx); err != nil {
		d.logger.Debug("inbound sip ack ignored", "error", err)
	}
}
```

- [ ] **Step 5: Extend OnBye for inbound dialogs**

Replace `onBye`:

```go
func (d *streamDialer) onBye(req *sip.Request, tx sip.ServerTransaction) {
	if err := d.out.ReadBye(req, tx); err == nil {
		d.logger.Info("remote sip stream ended")
		d.callbackMu.RLock()
		callback := d.remoteDialogEnded
		d.callbackMu.RUnlock()

		if callback != nil {
			go callback()
		}

		return
	}

	if d.in == nil {
		d.logger.Warn("remote sip bye rejected")
		return
	}

	if err := d.in.ReadBye(req, tx); err != nil {
		d.logger.Warn("remote sip bye rejected", "error", err)
		return
	}

	d.logger.Info("remote sip call ended")
	d.callbackMu.RLock()
	callback := d.remoteDialogEnded
	d.callbackMu.RUnlock()

	if callback != nil {
		go callback()
	}
}
```

- [ ] **Step 6: Use the configured identity**

In `NewStreamDialer`, replace the hardcoded fallbacks so `cfg.From`, `cfg.AuthUser`, `cfg.AuthPass`, `cfg.Listen` and `cfg.Transport` are honoured. `firstNonEmpty(cfg.From, "companion@127.0.0.1")` already does the right thing; verify `cfg.Listen` is used verbatim when non-empty and that `normalizeTransport(cfg.Transport)` is applied. No new code is needed here if those paths already exist — confirm by reading lines 69-146.

- [ ] **Step 7: Run tests**

Run: `go test ./internal/signaling/ -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/signaling/dialer.go internal/signaling/dialer_test.go
git commit -m "feat(signaling): handle inbound INVITE, CANCEL and ACK"
```

---

### Task 7: Call control API

**Files:**
- Modify: `internal/api/interfaces.go`
- Modify: `internal/api/api.go`
- Create: `internal/api/handlers_call.go`
- Test: `internal/api/api_test.go`

**Interfaces:**
- Produces:
  - `type CallControl interface { Answer(ctx context.Context) error; Hangup(ctx context.Context) error }`
  - `(*Server).SetCall(CallControl)`
  - `POST /api/v3/call/answer`, `POST /api/v3/call/hangup`, both bearer-protected.
- Consumes: `signaling.ErrNoIncomingDialog`.

- [ ] **Step 1: Write the failing test**

Append to `internal/api/api_test.go`, adapting `newTestServer` / request helpers to the ones already used in the file:

```go
type fakeCallControl struct {
	answers   int
	hangups   int
	answerErr error
}

func (c *fakeCallControl) Answer(context.Context) error {
	c.answers++
	return c.answerErr
}

func (c *fakeCallControl) Hangup(context.Context) error {
	c.hangups++
	return nil
}

func TestCallAnswerRoute(t *testing.T) {
	t.Parallel()

	server, token := newTestServer(t)
	call := &fakeCallControl{}
	server.SetCall(call)

	response := doRequest(t, server, http.MethodPost, "/api/v3/call/answer", token, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}

	if call.answers != 1 {
		t.Fatalf("answers = %d, want 1", call.answers)
	}
}

func TestCallAnswerConflictWhenCallGone(t *testing.T) {
	t.Parallel()

	server, token := newTestServer(t)
	server.SetCall(&fakeCallControl{answerErr: signaling.ErrNoIncomingDialog})

	response := doRequest(t, server, http.MethodPost, "/api/v3/call/answer", token, nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", response.Code, response.Body.String())
	}
}

func TestCallHangupRoute(t *testing.T) {
	t.Parallel()

	server, token := newTestServer(t)
	call := &fakeCallControl{}
	server.SetCall(call)

	response := doRequest(t, server, http.MethodPost, "/api/v3/call/hangup", token, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}

	if call.hangups != 1 {
		t.Fatalf("hangups = %d, want 1", call.hangups)
	}
}

func TestCallRoutesUnavailableWithoutController(t *testing.T) {
	t.Parallel()

	server, token := newTestServer(t)

	response := doRequest(t, server, http.MethodPost, "/api/v3/call/answer", token, nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}
```

If the file's helpers are named differently, use the existing ones — do not introduce new helpers.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestCall -v`
Expected: FAIL — `server.SetCall undefined`.

- [ ] **Step 3: Add the interface**

Append to `internal/api/interfaces.go`:

```go
type CallControl interface {
	Answer(ctx context.Context) error
	Hangup(ctx context.Context) error
}
```

- [ ] **Step 4: Add the field, setter and routes**

In `internal/api/api.go`, add to `Server`:

```go
	call CallControl
```

Add the setter next to the others:

```go
func (s *Server) SetCall(v CallControl) { s.call = v }
```

Register the routes next to the audio routes in `Handler`:

```go
	s.handleProtected(mux, "POST", "/api/v3/call/answer", s.answerCall)
	s.handleProtected(mux, "POST", "/api/v3/call/hangup", s.hangupCall)
```

- [ ] **Step 5: Write the handlers**

Create `internal/api/handlers_call.go`:

```go
package api

import (
	"bticino-go-companion/internal/signaling"
	"errors"
	"net/http"
)

func (s *Server) answerCall(w http.ResponseWriter, r *http.Request) {
	if s.call == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "call control is unavailable")
		return
	}

	if err := s.call.Answer(r.Context()); err != nil {
		if errors.Is(err, signaling.ErrNoIncomingDialog) {
			s.logger.InfoContext(r.Context(), "call answer rejected", "reason", "no incoming call")
			writeError(w, http.StatusConflict, "no_incoming_call", "there is no call to answer")

			return
		}

		s.logger.ErrorContext(r.Context(), "call answer failed", "error", err)
		writeCommandError(w, err)

		return
	}

	s.logger.InfoContext(r.Context(), "call answered")
	s.BroadcastState()
	writeOK(w, http.StatusOK, map[string]any{"state": s.currentPayload()})
}

// hangupCall is idempotent: the card also closes its WebRTC session, which
// tears the dialog down a second time.
func (s *Server) hangupCall(w http.ResponseWriter, r *http.Request) {
	if s.call == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "call control is unavailable")
		return
	}

	if err := s.call.Hangup(r.Context()); err != nil {
		s.logger.ErrorContext(r.Context(), "call hangup failed", "error", err)
		writeCommandError(w, err)

		return
	}

	s.logger.InfoContext(r.Context(), "call hung up")
	s.BroadcastState()
	writeOK(w, http.StatusOK, map[string]any{"state": s.currentPayload()})
}
```

- [ ] **Step 6: Run tests**

Run: `go test ./internal/api/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/api/interfaces.go internal/api/api.go internal/api/handlers_call.go internal/api/api_test.go
git commit -m "feat(api): expose call answer and hangup routes"
```

---

### Task 8: Wire a single shared Manager

**Files:**
- Modify: `internal/app/run.go` (`applicationRuntime`, `newRuntime`, `newSource`, `Run`)
- Test: `internal/app/run_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1, 4, 6 and 7.
- Produces: `applicationRuntime.calls *signaling.Manager`, shared by the API and by every media source.

- [ ] **Step 1: Write the failing test**

Append to `internal/app/run_test.go`:

```go
func TestResolveInboundEntrypointPrefersPhysicalRing(t *testing.T) {
	t.Parallel()

	entrypoints := []config.Entrypoint{
		{ID: "main", DevAddr: "20"},
		{ID: "side", DevAddr: "21"},
	}

	projector := core.NewProjector()
	if _, err := projector.Apply(core.RingStarted{EntrypointID: "side"}); err != nil {
		t.Fatal(err)
	}

	resolve := newInboundEntrypointResolver(func() []config.Entrypoint { return entrypoints }, projector)

	id, devAddr := resolve()
	if id != "side" || devAddr != "21" {
		t.Fatalf("resolve() = %q/%q, want side/21", id, devAddr)
	}
}

func TestResolveInboundEntrypointFallsBackToSoleEntrypoint(t *testing.T) {
	t.Parallel()

	entrypoints := []config.Entrypoint{{ID: "main", DevAddr: "20"}}
	resolve := newInboundEntrypointResolver(func() []config.Entrypoint { return entrypoints }, core.NewProjector())

	id, devAddr := resolve()
	if id != "main" || devAddr != "20" {
		t.Fatalf("resolve() = %q/%q, want main/20", id, devAddr)
	}
}

func TestResolveInboundEntrypointRefusesAmbiguity(t *testing.T) {
	t.Parallel()

	entrypoints := []config.Entrypoint{{ID: "main", DevAddr: "20"}, {ID: "side", DevAddr: "21"}}
	resolve := newInboundEntrypointResolver(func() []config.Entrypoint { return entrypoints }, core.NewProjector())

	if id, _ := resolve(); id != "" {
		t.Fatalf("resolve() = %q, want empty when the call cannot be attributed", id)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run TestResolveInboundEntrypoint -v`
Expected: FAIL — `undefined: newInboundEntrypointResolver`.

- [ ] **Step 3: Add the resolver**

Add to `internal/app/run.go`:

```go
// newInboundEntrypointResolver attributes an inbound SIP call to an entrypoint.
// The in-flight physical ring is the strongest signal; a single configured
// entrypoint is the safe fallback. Ambiguity is reported as "unattributable"
// so the manager rejects the call rather than inventing state.
func newInboundEntrypointResolver(entrypoints func() []config.Entrypoint, projector *core.Projector) signaling.EntrypointResolver {
	return func() (core.EntrypointID, string) {
		configured := entrypoints()

		if ring := projector.Snapshot().PhysicalRing; ring != nil {
			for _, entrypoint := range configured {
				if core.EntrypointID(entrypoint.ID) == ring.EntrypointID {
					return ring.EntrypointID, entrypoint.DevAddr
				}
			}
		}

		if len(configured) == 1 {
			return core.EntrypointID(configured[0].ID), configured[0].DevAddr
		}

		return "", ""
	}
}
```

- [ ] **Step 4: Build the shared Manager in newRuntime**

In `newRuntime`, replace the dialer construction:

```go
	sipConfig := initialConfig.Companion.SIP

	domain := strings.TrimSpace(sipConfig.Domain)
	if domain == "" {
		domain = signaling.DiscoverFlexisipDomain()
	}

	dialer, err := signaling.NewStreamDialer(signaling.StreamDialerConfig{
		Target:    mediaConfig.Target,
		Domain:    domain,
		From:      sipConfig.From,
		AuthUser:  sipConfig.AuthUser,
		AuthPass:  sipConfig.AuthPass,
		Transport: sipConfig.Transport,
		Listen:    sipConfig.Listen,
		Inbound:   sipConfig.Inbound,
		Logger:    logger,
	})
	if err != nil {
		return nil, fmt.Errorf("create sip runtime: %w", err)
	}
```

After `projector := core.NewProjector()`, create the Manager and connect it:

```go
	calls := signaling.NewManager("127.0.0.1", dialer, nil, newInboundEntrypointResolver(
		func() []config.Entrypoint { return configStore.Snapshot().Companion.Entrypoints },
		projector,
	))

	if sipConfig.Inbound {
		dialer.SetInboundHandler(calls)
	}
```

Note that `projector` must now be created **before** the `rtspServer` factory closure runs, i.e. move the `projector := core.NewProjector()` line above `rtspServer, err := media.NewRTSPServer(...)`. The factory closure only captures it; it does not dereference it at construction time.

Pass `calls` into the source factory:

```go
	rtspServer, err := media.NewRTSPServer(logger, media.DefaultRTSPAddress, initialConfig.Companion.Entrypoints, func(entrypoint config.Entrypoint, events media.SourceEvents) (media.ManagedSource, func(), error) {
		return newBridgeSource(configStore.Snapshot(), logger, dialer, calls, entrypoint, events, snapshots)
	})
```

Register it on the API server, only when inbound is enabled:

```go
	if sipConfig.Inbound {
		server.SetCall(calls)
	}
```

Add `calls` to the `applicationRuntime` struct and to the returned literal:

```go
	calls *signaling.Manager
```

- [ ] **Step 5: Stop creating a Manager per source**

Change `newBridgeSource` and `newSource` to accept the shared manager. In `newSource`, replace the `signaling.NewManager(...)` argument with the passed-in `calls`:

```go
func newSource(cfg config.Config, logger *slog.Logger, dialer signaling.StreamDialer, calls *signaling.Manager, entrypoint config.Entrypoint, videoPacket, audioPacket func(*rtp.Packet), remoteBYE func()) (*media.SourceSession, func(), error) {
```

and

```go
	source = media.NewSourceSession(
		logger,
		sourceConfig,
		core.EntrypointID(entrypoint.ID),
		calls,
		openwebnet.NewAVClient(logger),
		...
	)
```

Thread the new parameter through `newBridgeSource` to `newSource`.

- [ ] **Step 6: Connect the event sink**

In `Run`, immediately after `applyEvent := newEventApplier(projector, homeKit, server, logger)`:

```go
	runtime.calls.SetEvents(eventSinkFunc(applyEvent))
```

Add the adapter near `newEventApplier`:

```go
// eventSinkFunc adapts the applier closure to signaling.EventSink.
type eventSinkFunc func(core.Event)

func (f eventSinkFunc) Publish(event core.Event) { f(event) }
```

- [ ] **Step 7: Run the whole suite**

Run: `go test ./internal/...`
Expected: PASS. Fix any compile error introduced by the changed `newSource` signature.

- [ ] **Step 8: Build for the device**

Run: `go build ./...`
Expected: no output.

- [ ] **Step 9: Commit**

```bash
git add internal/app/run.go internal/app/run_test.go
git commit -m "feat(app): share one signaling manager across api and media"
```

---

## Phase B — Home Assistant integration

All tasks in this phase run in `D:/Progetti/BTicinoGO-Integration` on branch `feat/sip-incoming-call`.

### Task 9: API client methods

**Files:**
- Modify: `custom_components/bticino_companion/const.py`
- Modify: `custom_components/bticino_companion/api.py`
- Test: `tests/test_api_call.py` (create)

**Interfaces:**
- Produces: `CompanionClient.async_call_answer() -> dict[str, Any]`, `CompanionClient.async_call_hangup() -> dict[str, Any]`, constants `API_PATH_CALL_ANSWER` and `API_PATH_CALL_HANGUP`.

- [ ] **Step 1: Write the failing test**

Create `tests/test_api_call.py`:

```python
"""Companion call-control client methods."""

from __future__ import annotations

import pytest

from custom_components.bticino_companion.api import CompanionClient
from custom_components.bticino_companion.const import (
    API_PATH_CALL_ANSWER,
    API_PATH_CALL_HANGUP,
)


def _recording_client() -> tuple[CompanionClient, list[tuple[str, str, bool]]]:
    """Build a client without running __init__, so no aiohttp session is needed."""
    client = CompanionClient.__new__(CompanionClient)
    requests: list[tuple[str, str, bool]] = []

    async def _async_request(method, path, *, auth=False, json_body=None):
        requests.append((method, path, auth))
        return {"ok": True}

    client._async_request = _async_request  # type: ignore[method-assign]

    return client, requests


@pytest.mark.asyncio
async def test_async_call_answer_posts_to_answer_path() -> None:
    client, requests = _recording_client()

    assert await client.async_call_answer() == {"ok": True}
    assert requests == [("POST", API_PATH_CALL_ANSWER, True)]


@pytest.mark.asyncio
async def test_async_call_hangup_posts_to_hangup_path() -> None:
    client, requests = _recording_client()

    assert await client.async_call_hangup() == {"ok": True}
    assert requests == [("POST", API_PATH_CALL_HANGUP, True)]
```

Check `tests/test_websocket.py` for the repo's async test style. If it does not use
`pytest.mark.asyncio`, drop the decorators and drive the coroutines with `asyncio.run(...)` instead,
matching whatever that file already does.

- [ ] **Step 2: Run test to verify it fails**

Run: `python -m pytest tests/test_api_call.py -v`
Expected: FAIL — `ImportError: cannot import name 'API_PATH_CALL_ANSWER'`.

- [ ] **Step 3: Add the constants**

In `custom_components/bticino_companion/const.py`, after `API_PATH_AUDIO_UNMUTE`:

```python
API_PATH_CALL_ANSWER = "/api/v3/call/answer"
API_PATH_CALL_HANGUP = "/api/v3/call/hangup"
```

- [ ] **Step 4: Add the client methods**

In `custom_components/bticino_companion/api.py`, add both constants to the `from .const import (...)` block, then add after `async_unlock_entrypoint`:

```python
    async def async_call_answer(self) -> dict[str, Any]:
        """Answer the call currently ringing on Companion."""
        return await self._async_request("POST", API_PATH_CALL_ANSWER, auth=True)

    async def async_call_hangup(self) -> dict[str, Any]:
        """End the active call. Safe to call when no call is up."""
        return await self._async_request("POST", API_PATH_CALL_HANGUP, auth=True)
```

- [ ] **Step 5: Run tests**

Run: `python -m pytest tests/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add custom_components/bticino_companion/const.py custom_components/bticino_companion/api.py tests/test_api_call.py
git commit -m "feat(api): add call answer and hangup client methods"
```

---

### Task 10: State model exposes answerability

**Files:**
- Modify: `custom_components/bticino_companion/models.py`
- Modify: `custom_components/bticino_companion/camera.py`
- Test: `tests/test_models_protocol.py`

**Interfaces:**
- Produces: `CompanionState.last_call_end_reason: str | None`; camera attributes `bticino_can_answer: bool` and `bticino_answered_elsewhere: bool`.
- Consumes: the companion's `last_incoming_call_end` state field from Task 3.

- [ ] **Step 1: Write the failing test**

Append to `tests/test_models_protocol.py`:

```python
def test_state_reads_last_call_end_reason() -> None:
    state = CompanionState.from_dict(
        {
            "call_state": "ringing",
            "physical_ring": {"entrypoint_id": "main"},
            "last_incoming_call_end": {"dialog_id": "d1", "reason": "elsewhere"},
        }
    )

    assert state.last_call_end_reason == "elsewhere"


def test_state_without_call_end_reason_is_none() -> None:
    state = CompanionState.from_dict({"call_state": "idle"})

    assert state.last_call_end_reason is None


def test_incoming_dialog_id_marks_an_answerable_call() -> None:
    state = CompanionState.from_dict(
        {
            "call_state": "ringing",
            "incoming_call": {"dialog_id": "abc@domain", "entrypoint_id": "main"},
        }
    )

    assert state.incoming_dialog_id == "abc@domain"
    assert state.call_state == "ringing"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `python -m pytest tests/test_models_protocol.py -v`
Expected: FAIL — `AttributeError: 'CompanionState' object has no attribute 'last_call_end_reason'`.

- [ ] **Step 3: Extend the model**

In `custom_components/bticino_companion/models.py`, add the field to `CompanionState` after `active_dialog_id`:

```python
    last_call_end_reason: str | None = None
```

In `from_dict`, add near the other `mapping_at` calls:

```python
        last_call_end = mapping_at(payload, "last_incoming_call_end")
```

and in the `cls(...)` call, after `active_dialog_id=...`:

```python
            last_call_end_reason=_optional_string(last_call_end.get("reason")),
```

- [ ] **Step 4: Extend the camera attributes**

In `custom_components/bticino_companion/camera.py`, replace the returned dict in `extra_state_attributes`:

```python
        return {
            "bticino_entrypoint_id": self._entrypoint_id,
            "bticino_entrypoint_label": entrypoint.label if entrypoint and entrypoint.label else self._attr_name,
            "bticino_call_state": call_state,
            "bticino_is_active_entrypoint": active,
            "bticino_is_ringing": call_state == "ringing",
            # An answerable call requires a real SIP dialog. Without Flexisip
            # provisioning no INVITE arrives, incoming_dialog_id stays unset and
            # the card must not offer an Answer button that cannot work.
            "bticino_can_answer": bool(
                active and call_state == "ringing" and state and state.incoming_dialog_id
            ),
            "bticino_answered_elsewhere": bool(
                active and state and state.last_call_end_reason == "elsewhere"
            ),
        }
```

- [ ] **Step 5: Run tests**

Run: `python -m pytest tests/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add custom_components/bticino_companion/models.py custom_components/bticino_companion/camera.py tests/test_models_protocol.py
git commit -m "feat(camera): expose answerability and answered-elsewhere state"
```

---

### Task 11: Card WebSocket commands

**Files:**
- Modify: `custom_components/bticino_companion/websocket_api.py`
- Modify: `custom_components/bticino_companion/camera.py`

**Interfaces:**
- Produces: WebSocket commands `bticino_companion/card_call_answer` and `bticino_companion/card_call_hangup`, each taking only `camera_entity_id`; camera methods `async_handle_card_call_answer()` and `async_handle_card_call_hangup()`.

- [ ] **Step 1: Add the camera methods**

In `custom_components/bticino_companion/camera.py`, immediately after `async_handle_card_unlock`
(which is the exact pattern being mirrored — it reaches the REST client through `self._client`):

```python
    async def async_handle_card_call_answer(self) -> None:
        """Answer the call ringing on this camera's entrypoint."""
        await self._client.async_call_answer()

    async def async_handle_card_call_hangup(self) -> None:
        """End the active call. Idempotent on the Companion side."""
        await self._client.async_call_hangup()
```

No coordinator refresh is needed: the companion broadcasts its state over the WebSocket after both
routes, and the coordinator already applies those updates.

- [ ] **Step 2: Register the commands**

In `custom_components/bticino_companion/websocket_api.py`, add to `async_register_websocket_commands`:

```python
    websocket_api.async_register_command(hass, _handle_call_answer)
    websocket_api.async_register_command(hass, _handle_call_hangup)
```

Add the handlers after `_handle_unlock`:

```python
@callback
@websocket_api.websocket_command(
    {
        vol.Required("type"): "bticino_companion/card_call_answer",
        vol.Required("camera_entity_id"): str,
    }
)
@websocket_api.async_response
async def _handle_call_answer(hass: HomeAssistant, connection, msg: dict[str, Any]) -> None:
    """Answer the ringing call for the card's camera."""
    try:
        await _camera_for_message(hass, msg).async_handle_card_call_answer()
    except (CompanionApiError, HomeAssistantError, ValueError) as err:
        connection.send_error(msg["id"], "call_answer_failed", str(err))
        return
    connection.send_result(msg["id"])


@callback
@websocket_api.websocket_command(
    {
        vol.Required("type"): "bticino_companion/card_call_hangup",
        vol.Required("camera_entity_id"): str,
    }
)
@websocket_api.async_response
async def _handle_call_hangup(hass: HomeAssistant, connection, msg: dict[str, Any]) -> None:
    """End the active call for the card's camera."""
    try:
        await _camera_for_message(hass, msg).async_handle_card_call_hangup()
    except (CompanionApiError, HomeAssistantError, ValueError) as err:
        connection.send_error(msg["id"], "call_hangup_failed", str(err))
        return
    connection.send_result(msg["id"])
```

- [ ] **Step 2b: Verify it imports**

Run: `python -c "import ast,sys; ast.parse(open('custom_components/bticino_companion/websocket_api.py').read())"`
Expected: no output.

- [ ] **Step 3: Run tests**

Run: `python -m pytest tests/ -v`
Expected: PASS (unchanged count).

- [ ] **Step 4: Commit**

```bash
git add custom_components/bticino_companion/websocket_api.py custom_components/bticino_companion/camera.py
git commit -m "feat(websocket): add card call answer and hangup commands"
```

---

### Task 12: Card answer and hang-up buttons

**Files:**
- Modify: `custom_components/bticino_companion/www/bticino-go-intercom-card.js`

**Interfaces:**
- Consumes: `bticino_can_answer`, `bticino_answered_elsewhere`, `bticino_call_state` from Task 10; the two WebSocket commands from Task 11.

- [ ] **Step 1: Add the state readers**

After `_isRinging()`:

```js
  _canAnswer() {
    return this._cameraState()?.attributes?.bticino_can_answer === true;
  }

  _answeredElsewhere() {
    return this._cameraState()?.attributes?.bticino_answered_elsewhere === true;
  }
```

- [ ] **Step 2: Latch the answered-elsewhere notice**

The companion keeps `last_incoming_call_end` until the next call starts, so the card latches it for a few seconds instead of showing it indefinitely. In `set hass(hass)`, before `this._render()`:

```js
    if (this._answeredElsewhere() && !this._elsewhereUntil) {
      this._elsewhereUntil = Date.now() + 4000;
      setTimeout(() => { this._elsewhereUntil = 0; this._render(); }, 4000);
    }
```

Initialise it in the constructor:

```js
    this._elsewhereUntil = 0;
```

- [ ] **Step 3: Add the actions**

After `_unlock()`:

```js
  async _answer() {
    this._error = "";
    this._render();
    try {
      await this._send("bticino_companion/card_call_answer", {});
    } catch (err) {
      this._error = err?.message || String(err);
      this._render();
      return;
    }
    await this._start();
  }

  async _hangup() {
    try {
      await this._send("bticino_companion/card_call_hangup", {});
    } catch (err) {
      this._error = err?.message || String(err);
    }
    this._end();
  }
```

- [ ] **Step 4: Rework the buttons**

In `_render()`, after `const remoteActive = ...`, add:

```js
    const canAnswer = this._canAnswer() && !active;
    const inCall = callState === "active";
```

Add the answered-elsewhere status branch by inserting it as the first condition of the `status` ternary chain:

```js
    const status = this._elsewhereUntil
      ? ["Answered elsewhere", "idle"]
      : this._connecting
```

Replace the two call buttons:

```html
        <button class="call ${active || (ringing && !canAnswer) ? "hidden" : ""}" title="${canAnswer ? "Answer" : "Start live view"}"><ha-icon icon="mdi:phone"></ha-icon></button>
        <button class="end ${active ? "" : "hidden"}" title="${inCall ? "Hang up" : "End live view"}"><ha-icon icon="mdi:phone-hangup"></ha-icon></button>
```

Replace the two click bindings:

```js
    this.shadowRoot.querySelector(".call")?.addEventListener("click", () => (canAnswer ? this._answer() : this._start()));
    this.shadowRoot.querySelector(".end")?.addEventListener("click", () => (inCall ? this._hangup() : this._end()));
```

- [ ] **Step 5: Verify the syntax**

Run: `node --check custom_components/bticino_companion/www/bticino-go-intercom-card.js`
Expected: no output. If `node` is unavailable, open the file and confirm the three replaced regions are balanced.

- [ ] **Step 6: Commit**

```bash
git add custom_components/bticino_companion/www/bticino-go-intercom-card.js
git commit -m "feat(card): answer and hang up incoming calls"
```

---

## Phase C — Installer and documentation

### Task 13: Flexisip provisioning in the installer

**Files:**
- Modify: `scripts/install.sh`
- Modify: `docs/build-and-install.md`

This task runs in `D:/Progetti/BTicinoGO`.

**Interfaces:**
- Produces: shell functions `flexisip_domain`, `provision_flexisip_user`, `enable_sip_inbound`; environment switch `COMPANION_SIP_INBOUND=0` to skip provisioning.

- [ ] **Step 1: Add the provisioning functions**

Insert into `scripts/install.sh` before `post_install_checks`:

```sh
FLEXISIP_USERS_DB="/etc/flexisip/users/users.db.txt"
FLEXISIP_ROUTE="/etc/flexisip/users/route.conf"
FLEXISIP_ROUTE_INT="/etc/flexisip/users/route_int.conf"
FLEXISIP_DOMAIN_REG="/etc/flexisip/domain-registration.conf"
SIP_USER="companion"

flexisip_domain() {
	if [ -r "${FLEXISIP_DOMAIN_REG}" ]; then
		awk 'NF { print $1; exit }' "${FLEXISIP_DOMAIN_REG}" && return 0
	fi
	for conf in /etc/flexisip/flexisip.conf /home/bticino/cfg/flexisip.conf; do
		[ -r "${conf}" ] || continue
		awk -F= '/^[[:space:]]*(aliases|reg-domains|auth-domains)=/ { print $2; exit }' "${conf}" | awk '{ print $1; exit }' && return 0
	done
	return 1
}

backup_once() {
	[ -f "$1" ] || return 1
	[ -f "$1.companion.bak" ] || cp -p "$1" "$1.companion.bak"
}

provision_flexisip_user() {
	domain="$1"
	for file in "${FLEXISIP_USERS_DB}" "${FLEXISIP_ROUTE}" "${FLEXISIP_ROUTE_INT}"; do
		if ! backup_once "${file}"; then
			warn "Missing ${file}; skipping SIP inbound provisioning."
			return 1
		fi
	done

	if ! grep -q "^${SIP_USER}@${domain} " "${FLEXISIP_USERS_DB}"; then
		hash_line="$(awk -v d="@${domain}" 'index($1, d) { print $2; exit }' "${FLEXISIP_USERS_DB}")"
		if [ -z "${hash_line}" ]; then
			warn "No existing user hash found in ${FLEXISIP_USERS_DB}; skipping SIP inbound provisioning."
			return 1
		fi
		printf '%s@%s %s ;\n' "${SIP_USER}" "${domain}" "${hash_line}" >> "${FLEXISIP_USERS_DB}"
		log "Added ${SIP_USER}@${domain} to users.db.txt"
	fi

	if ! grep -q "sip:${SIP_USER}@${domain}" "${FLEXISIP_ROUTE}"; then
		printf '<sip:%s@%s> <sip:127.0.0.1>\n' "${SIP_USER}" "${domain}" >> "${FLEXISIP_ROUTE}"
		log "Added ${SIP_USER}@${domain} to route.conf"
	fi

	if grep -q "sip:${SIP_USER}@${domain}" "${FLEXISIP_ROUTE_INT}"; then
		ok "SIP inbound already provisioned."
		return 0
	fi

	tmp="${FLEXISIP_ROUTE_INT}.companion.tmp"
	sed "s|\(<sip:alluser@${domain}>.*\)$|\1, <sip:${SIP_USER}@${domain}>|" "${FLEXISIP_ROUTE_INT}" > "${tmp}"

	# route_int.conf must stay one line per group; a mangled file breaks the
	# whole intercom, so validate before replacing the original.
	if [ "$(grep -c "sip:alluser@${domain}" "${tmp}")" -ne 1 ] || ! grep -q "sip:${SIP_USER}@${domain}" "${tmp}"; then
		rm -f "${tmp}"
		warn "route_int.conf rewrite failed validation; original left untouched."
		return 1
	fi

	mv "${tmp}" "${FLEXISIP_ROUTE_INT}"
	log "Added ${SIP_USER}@${domain} to the alluser group"

	return 0
}

enable_sip_inbound() {
	config="${BASE_DIR}/config.yaml"
	[ -f "${config}" ] || { warn "Companion config not found at ${config}; enable companion.sip.inbound manually."; return 1; }
	if grep -q '^[[:space:]]*inbound:' "${config}"; then
		sed -i 's/^\([[:space:]]*\)inbound:.*/\1inbound: true/' "${config}"
	else
		warn "companion.sip section absent from ${config}; enable companion.sip.inbound manually after first start."
		return 1
	fi
	ok "Enabled companion.sip.inbound"

	return 0
}

setup_sip_inbound() {
	if [ "${COMPANION_SIP_INBOUND:-1}" = "0" ]; then
		log "Skipping SIP inbound provisioning (COMPANION_SIP_INBOUND=0)."
		return 0
	fi

	domain="$(flexisip_domain || true)"
	if [ -z "${domain}" ]; then
		warn "Could not determine the Flexisip domain; skipping SIP inbound provisioning."
		return 0
	fi

	log "Provisioning SIP inbound for ${SIP_USER}@${domain}"
	provision_flexisip_user "${domain}" && enable_sip_inbound || true

	return 0
}
```

- [ ] **Step 2: Call it from main**

In `main`, between `start_service` and `restore_root_ro`, the companion must already have written its config once. Change the sequence to:

```sh
	start_service; wait_for_health "$(health_url)" "${HEALTHCHECK_TIMEOUT_SEC}" || true
	setup_sip_inbound; start_service; restore_root_ro; post_install_checks
```

The first `start_service` creates `config.yaml`; `setup_sip_inbound` then edits it and the second `start_service` restarts the companion so it picks up `inbound: true`.

- [ ] **Step 3: Check the script parses**

Run: `sh -n scripts/install.sh`
Expected: no output.

- [ ] **Step 4: Document the limitation**

Append to `docs/build-and-install.md`:

```markdown
## Answering calls from Home Assistant

The installer provisions a `companion@<domain>` SIP user in Flexisip so the
intercom forks incoming calls to the companion. Run the installer with
`COMPANION_SIP_INBOUND=0` to skip this and leave the BTicino files untouched.

> **Known limitation.** Logging out and back in from the BTicino DoorEntry app
> rewrites `/etc/flexisip/users/*` and removes the `companion` user. Answering
> then stops working **silently** — the card shows the ring but offers no Answer
> button. Re-run the installer to restore it. Backups of the three files are
> kept alongside them with a `.companion.bak` suffix.
```

- [ ] **Step 5: Commit**

```bash
git add scripts/install.sh docs/build-and-install.md
git commit -m "feat(install): provision the companion SIP user in flexisip"
```

---

### Task 14: On-device verification

**Files:** none — this task produces a verification record, not code.

- [ ] **Step 1: Install and confirm registration**

On the intercom:

```sh
sh install.sh
grep companion /etc/flexisip/users/route_int.conf
grep 'inbound:' /home/bticino/cfg/extra/companion/config.yaml
```

Expected: the `alluser` line contains `<sip:companion@…>`, and `inbound: true`.

- [ ] **Step 2: Capture a real ring**

```sh
tcpdump -i lo -s0 -A port 5060 -w /tmp/ring.pcap &
# press the outdoor station button, wait for the ring, then:
kill %1
tcpdump -r /tmp/ring.pcap -A | head -200
```

Expected: an `INVITE sip:companion@<domain>` arriving, followed by our `SIP/2.0 180 Ringing`.

- [ ] **Step 3: Answer from the card and confirm the 200 OK is accepted**

Press Answer in Home Assistant while the capture is running. Expected in the capture: our `SIP/2.0 200 OK` with the `a=DEVADDR:` SDP, then an `ACK` from the proxy. Expected in the companion log: `call answered`, then `av stream flowing` for both video and audio.

- [ ] **Step 4: Record the outcome**

If the `200 OK` is rejected, extract from the same capture the `200 OK` that the physical monitor sends and open an issue with both SDPs attached. That capture is the input for correcting `BuildAnswer`; see §11 of the spec.

- [ ] **Step 5: Confirm the monitor stops ringing**

Note in the issue or commit message whether the physical monitor fell silent after the card answered. The spec records this as an expectation, not a guarantee.

---

## Self-review notes

- Spec §5.4's removal of `CallHungUp` from the mapper is covered by Task 5; the Manager publishes it in `Hangup` (Task 4).
- Spec §10's "media start fails after a successful answer" is satisfied without extra code: `answer` never touches the media path (Task 7), so a WebRTC failure surfaces in the card without ending the call.
- Spec §6's `bticino_can_answer` gating is the only guard against a missing Flexisip provisioning, per the "installer only, no detection" decision.

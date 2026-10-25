# Install from Home Assistant — Companion Side Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the companion what Home Assistant needs to install and pair it over SSH — a loopback-only `POST /api/v3/pair/local` that can also set the web UI owner — and add `scripts/uninstall.sh`.

**Architecture:** The owner rules and the owner write move from the web UI handler into `internal/auth` (`ValidateOwner`, `Store.SetupOwner`), so the web UI bootstrap and the new API route share them. `pair/local` lives in `internal/api` next to the other pairing routes and only answers loopback peers. `install.sh` records the firewall ports it adds so `uninstall.sh` can remove exactly those; both scripts get logic tests that run unprivileged under any POSIX `sh`.

**Tech Stack:** Go 1.x (`net/netip`, `golang.org/x/crypto/bcrypt`, already in `go.mod`), POSIX `sh` (busybox ash on the device).

**Spec:** `docs/superpowers/specs/2026-10-22-ha-driven-install-design.md`, sections 4.1–4.3.

## Global Constraints

- Branch: `feat/ha-driven-install` (already checked out, on top of `fix/installer-hardening`).
- `dist/companion` is modified in the working tree and must never be committed.
- Go tests do not run on this Windows host (see `docs/build-and-install.md` §5.3). Verify every Go task with `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./...`; run the tests with `docker build --target test .` only if Docker is running, otherwise say plainly that they were written but not run. Do **not** cross-compile test binaries to run them under WSL.
- Shell scripts: POSIX `sh` only (busybox ash on the device), tabs for indentation, LF line endings, same `INFO:`/`OK:`/`WARN:`/`FAIL:` output as `install.sh`.
- Commit messages: conventional style used in the repo (`feat(api): …`, `fix(install): …`), ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Web UI bootstrap credentials are `companion`/`companion`; an owner may use neither; passwords need at least 8 characters.

---

### Task 1: Owner rules and `Store.SetupOwner` in `internal/auth`

**Files:**
- Create: `internal/auth/owner.go`
- Create: `internal/auth/owner_test.go`
- Modify: `internal/auth/store_test.go:258-287` (split `newTestStore`)

**Interfaces:**
- Produces:
  - `const auth.DefaultOwnerUsername = "companion"`, `const auth.DefaultOwnerPassword = "companion"`
  - `var auth.ErrInvalidOwner error`
  - `func auth.ValidateOwner(username, password string) error` — returns an error wrapping `ErrInvalidOwner` whose message names the rule.
  - `func (s *auth.Store) SetupOwner(username, password string) (string, error)` — returns the claim code; `ErrClaimNotAllowed` unless `setup_required`; `ErrInvalidOwner` (wrapped) for bad credentials; `ErrStoreUnavailable` without a config.

- [ ] **Step 1: Write the failing tests**

Create `internal/auth/owner_test.go`:

```go
package auth

import (
	"bticino-go-companion/internal/config"
	"errors"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestValidateOwner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		username, password string
		valid              bool
	}{
		{name: "valid", username: "owner", password: "correct-horse", valid: true},
		{name: "blank username", username: "  ", password: "correct-horse"},
		{name: "short password", username: "owner", password: "short"},
		{name: "default username", username: DefaultOwnerUsername, password: "correct-horse"},
		{name: "default password", username: "owner", password: DefaultOwnerPassword},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateOwner(tt.username, tt.password)
			if tt.valid && err != nil {
				t.Fatalf("ValidateOwner() error = %v, want nil", err)
			}

			if !tt.valid && !errors.Is(err, ErrInvalidOwner) {
				t.Fatalf("ValidateOwner() error = %v, want ErrInvalidOwner", err)
			}
		})
	}
}

func TestStore_SetupOwner(t *testing.T) {
	t.Parallel()

	store, backend := newSetupRequiredStore(t)

	code, err := store.SetupOwner(" owner ", "correct-horse")
	if err != nil {
		t.Fatalf("SetupOwner() error = %v", err)
	}

	cfg := backend.Snapshot()
	if cfg.Auth.PairingState != config.PairingStateClaimable {
		t.Fatalf("pairing state = %q, want claimable", cfg.Auth.PairingState)
	}

	if cfg.WebUI.AdminUsername != "owner" {
		t.Fatalf("admin username = %q, want trimmed %q", cfg.WebUI.AdminUsername, "owner")
	}

	if bcrypt.CompareHashAndPassword([]byte(cfg.WebUI.AdminPasswordHash), []byte("correct-horse")) != nil {
		t.Fatal("admin password hash does not match the owner password")
	}

	if cfg.WebUI.SessionSecret == "" {
		t.Fatal("owner setup left the session secret empty")
	}

	want, err := store.InitialClaimCode()
	if err != nil || code != want || !config.ValidClaimCode(code) {
		t.Fatalf("claim code = %q (InitialClaimCode %q, %v)", code, want, err)
	}

	if _, err := store.SetupOwner("other", "correct-horse"); !errors.Is(err, ErrClaimNotAllowed) {
		t.Fatalf("second SetupOwner() error = %v, want ErrClaimNotAllowed", err)
	}
}

func TestStore_SetupOwnerRejectsInvalidOwnerWithoutChangingConfig(t *testing.T) {
	t.Parallel()

	store, backend := newSetupRequiredStore(t)
	before := backend.Snapshot()

	if _, err := store.SetupOwner("owner", "short"); !errors.Is(err, ErrInvalidOwner) {
		t.Fatalf("SetupOwner() error = %v, want ErrInvalidOwner", err)
	}

	after := backend.Snapshot()
	if after.Auth.PairingState != config.PairingStateSetupRequired || after.WebUI != before.WebUI {
		t.Fatalf("rejected owner changed the config: %#v", after)
	}
}
```

In `internal/auth/store_test.go`, replace the whole `newTestStore` function (lines 258–287) with these two functions, so the existing tests keep their `claimable` store:

```go
func newTestStore(t *testing.T) (*Store, *config.Store) {
	t.Helper()

	store, backend := newSetupRequiredStore(t)

	if err := backend.Update(func(cfg *config.Config) error {
		cfg.WebUI.SessionSecret = "test-session-secret"
		return nil
	}); err != nil {
		t.Fatalf("set session secret: %v", err)
	}

	if _, err := store.StartInitialClaim(); err != nil {
		t.Fatalf("issue initial claim code: %v", err)
	}

	return store, backend
}

func newSetupRequiredStore(t *testing.T) (*Store, *config.Store) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.Create(path, config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"}); err != nil {
		t.Fatalf("create config: %v", err)
	}

	backend, err := config.Open(path)
	if err != nil {
		t.Fatalf("open config store: %v", err)
	}

	store := NewStore(backend)
	store.now = func() time.Time { return testNow }

	return store, backend
}
```

- [ ] **Step 2: Verify the tests do not compile yet**

Run: `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/auth/`
Expected: FAIL with `undefined: ValidateOwner`, `undefined: ErrInvalidOwner`, `undefined: DefaultOwnerUsername` and `store.SetupOwner undefined`.

- [ ] **Step 3: Implement**

Create `internal/auth/owner.go`:

```go
package auth

import (
	"bticino-go-companion/internal/config"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// DefaultOwnerUsername and DefaultOwnerPassword are the web UI's bootstrap
// credentials. They only open the owner setup, so an owner may use neither.
const (
	DefaultOwnerUsername = "companion"
	DefaultOwnerPassword = "companion"

	minOwnerPasswordLength = 8
)

var ErrInvalidOwner = errors.New("invalid owner credentials")

// ValidateOwner reports the first rule the owner credentials break. The
// username is compared trimmed, as it is stored.
func ValidateOwner(username, password string) error {
	username = strings.TrimSpace(username)

	switch {
	case username == "":
		return fmt.Errorf("%w: username is required", ErrInvalidOwner)
	case len(password) < minOwnerPasswordLength:
		return fmt.Errorf("%w: password must contain at least %d characters", ErrInvalidOwner, minOwnerPasswordLength)
	case username == DefaultOwnerUsername:
		return fmt.Errorf("%w: username must replace the default", ErrInvalidOwner)
	case password == DefaultOwnerPassword:
		return fmt.Errorf("%w: password must replace the default", ErrInvalidOwner)
	}

	return nil
}

// SetupOwner sets the web UI owner of a companion that has none and opens the
// initial claim, in a single config update, and returns the claim code. It is
// the owner setup the web UI does on its first login, available to callers
// that already proved they own the device.
func (s *Store) SetupOwner(username, password string) (string, error) {
	if s.config == nil {
		return "", ErrStoreUnavailable
	}

	username = strings.TrimSpace(username)
	if err := ValidateOwner(username, password); err != nil {
		return "", err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash owner password: %w", err)
	}

	secret, err := config.RandomHex(32)
	if err != nil {
		return "", fmt.Errorf("generate session secret: %w", err)
	}

	if err := s.config.Update(func(cfg *config.Config) error {
		if cfg.Auth.PairingState != config.PairingStateSetupRequired {
			return ErrClaimNotAllowed
		}

		cfg.WebUI.AdminUsername = username
		cfg.WebUI.AdminPasswordHash = string(hash)
		cfg.WebUI.SessionSecret = secret
		cfg.Auth.PairingState = config.PairingStateClaimable

		return nil
	}); err != nil {
		return "", err
	}

	return s.InitialClaimCode()
}
```

- [ ] **Step 4: Verify it compiles and vets clean**

Run: `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/auth/`
Expected: no output, exit 0.

If Docker is running, also run `docker build --target test .` and expect the `auth` tests to pass. Otherwise record that the tests were written but not run.

- [ ] **Step 5: Commit**

```bash
git add internal/auth/owner.go internal/auth/owner_test.go internal/auth/store_test.go
git commit -m "feat(auth): validate and set the web UI owner from the auth store" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: The web UI bootstrap uses `SetupOwner`

**Files:**
- Modify: `internal/webui/server.go` (constants at lines 29–30, `handleLogin` around line 359, `handlePassword` lines 453–541)
- Test: `internal/webui/server_test.go`

**Interfaces:**
- Consumes: `auth.ValidateOwner`, `auth.DefaultOwnerUsername`, `auth.DefaultOwnerPassword`, `(*auth.Store).SetupOwner`, `(*auth.Store).PairingState` (Task 1).
- Produces: nothing new for later tasks.

- [ ] **Step 1: Write the failing test**

A bootstrap on a companion that is already `claimable` (owner never set) used to fail with 500, because `StartInitialClaim` returned `ErrClaimNotAllowed`. Add to `internal/webui/server_test.go`, next to `TestBootstrapOwnerSetupIssuesClaimCodeAndCanIssueRepairCode`:

```go
func TestBootstrapOwnerSetupOnClaimableCompanionKeepsItsClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	cfg, err := config.Default(config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"})
	if err != nil {
		t.Fatal(err)
	}

	if err := writeConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	store, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	authStore := auth.NewStore(store)
	if err := store.Update(func(cfg *config.Config) error {
		cfg.WebUI.SessionSecret = "test-session-secret"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := authStore.StartInitialClaim(); err != nil {
		t.Fatal(err)
	}

	server := New(store, authStore, slog.New(slog.DiscardHandler), nil, nil, func(string) error { return nil })

	login := request(t, server, http.MethodPost, "/webui/api/login", loginRequest{Username: auth.DefaultOwnerUsername, Password: auth.DefaultOwnerPassword}, nil)
	setup := request(t, server, http.MethodPost, "/webui/api/bootstrap/account", passwordRequest{Username: "admin", Password: "correct-horse", PasswordConfirm: "correct-horse"}, sessionCookieFrom(t, login))

	if setup.Code != http.StatusOK {
		t.Fatalf("owner setup on a claimable companion = %d: %s", setup.Code, setup.Body.String())
	}

	if state := store.Snapshot().Auth.PairingState; state != config.PairingStateClaimable {
		t.Fatalf("pairing state = %q, want claimable", state)
	}

	passwordSession(t, server, "correct-horse")
}
```

- [ ] **Step 2: Verify it fails to compile against the old constants, then fails at run time**

Run: `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/webui/`
Expected: exit 0 (the test only uses names that exist after Task 1). It would fail at run time with `owner setup on a claimable companion = 500`; that is observable only where the tests run (Docker/CI).

- [ ] **Step 3: Implement**

In `internal/webui/server.go`:

1. Delete the two constants `defaultUsername = "companion"` and `defaultPassword = "companion"` (lines 29–30) and replace every use of them in `server.go` **and** `server_test.go` with `auth.DefaultOwnerUsername` / `auth.DefaultOwnerPassword` (use `grep -n "defaultUsername\|defaultPassword" internal/webui/*.go` to find them all).

2. Replace `handlePassword` (from `func (s *Server) handlePassword` to the closing brace before `func (s *Server) handleEntrypointsConfig`) with:

```go
func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}

	var request passwordRequest
	if !decodeJSON(w, r, &request) {
		return
	}

	cfg, ok := s.snapshot()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "configuration is unavailable")
		return
	}

	current, authenticated := s.currentSession(r, cfg)
	if !authenticated {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	refreshSessionCookie(w, r)

	request.Username = strings.TrimSpace(request.Username)

	invalid := request.Username == "" || len(request.Password) < 8
	if current.bootstrap {
		invalid = auth.ValidateOwner(request.Username, request.Password) != nil
	}

	if invalid {
		writeError(w, http.StatusBadRequest, "password must replace the default and contain at least 8 characters")
		return
	}

	if current.bootstrap && request.Password != request.PasswordConfirm {
		writeError(w, http.StatusBadRequest, "password confirmation does not match")
		return
	}

	if !current.bootstrap && bcrypt.CompareHashAndPassword([]byte(cfg.WebUI.AdminPasswordHash), []byte(request.CurrentPassword)) != nil {
		writeError(w, http.StatusUnauthorized, "current password is invalid")
		return
	}

	if current.bootstrap && s.auth != nil && s.auth.PairingState() == config.PairingStateSetupRequired {
		if _, err := s.auth.SetupOwner(request.Username, request.Password); err != nil {
			s.logger.Error("webui owner setup", "error", err)
			writeError(w, http.StatusInternalServerError, "save password failed")

			return
		}
	} else if !s.savePassword(w, request) {
		return
	}

	s.mu.Lock()
	s.sessions = make(map[string]session)
	s.mu.Unlock()
	clearSessionCookie(w)

	if current.bootstrap {
		s.logger.Info("webui owner setup completed")
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// savePassword writes the owner credentials without touching the pairing
// state: a password change, or a bootstrap on a companion whose pairing moved
// on before an owner was ever set.
func (s *Server) savePassword(w http.ResponseWriter, request passwordRequest) bool {
	hash, err := bcrypt.GenerateFromPassword([]byte(request.Password), bcrypt.DefaultCost)
	if err != nil {
		s.logger.Error("hash webui password", "error", err)
		writeError(w, http.StatusInternalServerError, "save password failed")

		return false
	}

	secret, err := config.RandomHex(32)
	if err != nil {
		s.logger.Error("create webui session secret", "error", err)
		writeError(w, http.StatusInternalServerError, "save password failed")

		return false
	}

	if err := s.config.Update(func(cfg *config.Config) error {
		cfg.WebUI.AdminUsername = request.Username
		cfg.WebUI.AdminPasswordHash = string(hash)
		cfg.WebUI.SessionSecret = secret

		return nil
	}); err != nil {
		s.logger.Error("save webui password", "error", err)
		writeError(w, http.StatusInternalServerError, "save password failed")

		return false
	}

	return true
}
```

The `StartInitialClaim` call that used to follow the config update is gone: on `setup_required` the claim is opened by `SetupOwner`, and in any other state it could only fail.

- [ ] **Step 4: Verify**

Run: `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/webui/ ./internal/auth/`
Expected: no output, exit 0.
Run: `grep -n "defaultUsername\|defaultPassword" internal/webui/*.go`
Expected: no matches.
If Docker is running: `docker build --target test .` — all `webui` tests pass, including `TestBootstrapOwnerSetupIssuesClaimCodeAndCanIssueRepairCode` and the new test.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/server.go internal/webui/server_test.go
git commit -m "refactor(webui): set the bootstrap owner through the auth store" -m "A bootstrap on a companion that is already claimable no longer fails with 500." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: `POST /api/v3/pair/local`

**Files:**
- Modify: `internal/api/api.go` (route list in `Handler()` around line 81; new handler after `pairClaim`; helper after `sourceIP`; import `net/netip`)
- Test: `internal/api/api_test.go` (new test; split `newTestServer` at line 767)

**Interfaces:**
- Consumes: `(*auth.Store).SetupOwner`, `auth.ErrInvalidOwner` (Task 1); existing `InitialClaimCode`, `IssueRepairCode`, `PairingState`.
- Produces (HTTP contract used by the Home Assistant plan):
  - `POST /api/v3/pair/local`, body `{}` or `{"owner":{"username":"…","password":"…"}}`.
  - 200 `{"ok":true,"device_id":…,"instance_id":…,"pairing_state":"claimable","claim_code":"xxxx-xxxx"}`
  - 200 `{"ok":true,"device_id":…,"instance_id":…,"pairing_state":"claimed","repair_code":"…","repair_expires_at":"<RFC 3339>"}`
  - 400 `{"ok":false,"error":{"code":"invalid_owner","message":"invalid owner credentials: <rule>"}}`
  - 409 `{"ok":false,"error":{"code":"pairing_error",…}}`
  - 404 `{"ok":false,"error":{"code":"not_found",…}}` from any non-loopback peer.

- [ ] **Step 1: Write the failing test**

In `internal/api/api_test.go`, replace `newTestServer` (line 767 to its closing brace) with:

```go
func newTestServer(t *testing.T) (*Server, *config.Store) {
	t.Helper()

	server, store := newSetupRequiredTestServer(t)
	if err := store.Update(func(cfg *config.Config) error {
		cfg.WebUI.SessionSecret = "test-session-secret"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := server.auth.StartInitialClaim(); err != nil {
		t.Fatal(err)
	}

	return server, store
}

func newSetupRequiredTestServer(t *testing.T) (*Server, *config.Store) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.Create(path, config.Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"}); err != nil {
		t.Fatal(err)
	}

	store, err := config.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	return NewServer(auth.NewStore(store), store, core.NewProjector(), slog.New(slog.DiscardHandler)), store
}
```

Add after `TestServer_RepairRecovery`:

```go
type pairLocalResponse struct {
	DeviceID        string `json:"device_id"`
	InstanceID      string `json:"instance_id"`
	PairingState    string `json:"pairing_state"`
	ClaimCode       string `json:"claim_code"`
	RepairCode      string `json:"repair_code"`
	RepairExpiresAt string `json:"repair_expires_at"`
	Error           struct {
		Code string `json:"code"`
	} `json:"error"`
}

func TestServer_PairLocal(t *testing.T) {
	t.Parallel()

	owner := `{"owner":{"username":"owner","password":"correct-horse"}}`

	t.Run("hidden from the network", func(t *testing.T) {
		t.Parallel()

		server, _ := newTestServer(t)
		response, body := pairLocal(t, server, "192.0.2.1:1234", owner)

		if response.Code != http.StatusNotFound || body.Error.Code != "not_found" {
			t.Fatalf("remote pair/local = %d: %s", response.Code, response.Body.String())
		}
	})

	t.Run("setup required sets the owner and opens the claim", func(t *testing.T) {
		t.Parallel()

		server, store := newSetupRequiredTestServer(t)
		response, body := pairLocal(t, server, "127.0.0.1:5555", owner)

		want, err := server.auth.InitialClaimCode()
		if err != nil {
			t.Fatal(err)
		}

		cfg := store.Snapshot()
		if response.Code != http.StatusOK || body.PairingState != "claimable" || body.ClaimCode != want {
			t.Fatalf("pair/local = %d: %s", response.Code, response.Body.String())
		}

		if body.DeviceID != cfg.Companion.DeviceID || body.InstanceID != cfg.Auth.InstanceID {
			t.Fatalf("identity = %q/%q, want %q/%q", body.DeviceID, body.InstanceID, cfg.Companion.DeviceID, cfg.Auth.InstanceID)
		}

		if cfg.WebUI.AdminUsername != "owner" {
			t.Fatalf("admin username = %q, want owner", cfg.WebUI.AdminUsername)
		}
	})

	t.Run("setup required refuses a missing or invalid owner", func(t *testing.T) {
		t.Parallel()

		for _, request := range []string{`{}`, `{"owner":{"username":"owner","password":"companion"}}`} {
			server, store := newSetupRequiredTestServer(t)
			response, body := pairLocal(t, server, "127.0.0.1:5555", request)

			if response.Code != http.StatusBadRequest || body.Error.Code != "invalid_owner" {
				t.Fatalf("pair/local %s = %d: %s", request, response.Code, response.Body.String())
			}

			if state := store.Snapshot().Auth.PairingState; state != config.PairingStateSetupRequired {
				t.Fatalf("pairing state after %s = %q, want setup_required", request, state)
			}
		}
	})

	t.Run("claimable returns the claim code and ignores the owner", func(t *testing.T) {
		t.Parallel()

		server, store := newTestServer(t)
		response, body := pairLocal(t, server, "[::1]:5555", owner)

		want, err := server.auth.InitialClaimCode()
		if err != nil {
			t.Fatal(err)
		}

		if response.Code != http.StatusOK || body.PairingState != "claimable" || body.ClaimCode != want {
			t.Fatalf("pair/local = %d: %s", response.Code, response.Body.String())
		}

		if store.Snapshot().WebUI.AdminUsername != "" {
			t.Fatal("pair/local set an owner on a claimable companion")
		}
	})

	t.Run("claimed issues a repair code that recovers the bearer", func(t *testing.T) {
		t.Parallel()

		server, _ := newTestServer(t)
		if _, err := server.auth.RotateBearer(); err != nil {
			t.Fatal(err)
		}

		response, body := pairLocal(t, server, "127.0.0.1:5555", `{}`)
		if response.Code != http.StatusOK || body.PairingState != "claimed" || body.RepairCode == "" {
			t.Fatalf("pair/local = %d: %s", response.Code, response.Body.String())
		}

		if _, err := time.Parse(time.RFC3339, body.RepairExpiresAt); err != nil {
			t.Fatalf("repair_expires_at = %q: %v", body.RepairExpiresAt, err)
		}

		if _, err := server.auth.RecoverBearer(body.RepairCode); err != nil {
			t.Fatalf("recover with the local repair code: %v", err)
		}
	})
}

func pairLocal(t *testing.T, server *Server, remoteAddr, body string) (*httptest.ResponseRecorder, pairLocalResponse) {
	t.Helper()

	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v3/pair/local", strings.NewReader(body))
	request.RemoteAddr = remoteAddr
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	var decoded pairLocalResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode pair/local response %q: %v", response.Body.String(), err)
	}

	return response, decoded
}
```

- [ ] **Step 2: Verify it does not compile... or fails**

Run: `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./internal/api/`
Expected: exit 0 (the test uses only existing names); at run time every subtest except "hidden from the network" fails with 404, because the route does not exist.

- [ ] **Step 3: Implement**

In `internal/api/api.go`:

1. Add `"net/netip"` to the imports (after `"net/http"`).
2. In `Handler()`, after `s.handle(mux, "POST", "/api/v3/pair/claim", s.pairClaim)`, add:

```go
	s.handle(mux, "POST", "/api/v3/pair/local", s.pairLocal)
```

3. After `pairClaim`, add:

```go
// pairLocal hands a claim or repair code to a caller on the intercom itself,
// which already proved it owns the device by logging in as root. From the
// network the route does not exist.
func (s *Server) pairLocal(w http.ResponseWriter, r *http.Request) {
	if !fromLoopback(r) {
		s.notFound(w, r)
		return
	}

	var body struct {
		Owner *struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"owner"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	if s.auth == nil || s.config == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "authentication is unavailable")
		return
	}

	cfg := s.config.Snapshot()
	payload := map[string]any{"device_id": cfg.Companion.DeviceID, "instance_id": cfg.Auth.InstanceID}

	switch s.auth.PairingState() {
	case config.PairingStateSetupRequired:
		if body.Owner == nil {
			writeError(w, http.StatusBadRequest, "invalid_owner", "owner credentials are required")
			return
		}

		code, err := s.auth.SetupOwner(body.Owner.Username, body.Owner.Password)
		if errors.Is(err, auth.ErrInvalidOwner) {
			writeError(w, http.StatusBadRequest, "invalid_owner", err.Error())
			return
		}

		if err != nil {
			writeAuthError(w, err)
			return
		}

		payload["pairing_state"] = config.PairingStateClaimable
		payload["claim_code"] = code
	case config.PairingStateClaimable:
		code, err := s.auth.InitialClaimCode()
		if err != nil {
			writeAuthError(w, err)
			return
		}

		payload["pairing_state"] = config.PairingStateClaimable
		payload["claim_code"] = code
	case config.PairingStateClaimed:
		code, expiresAt, err := s.auth.IssueRepairCode()
		if err != nil {
			writeAuthError(w, err)
			return
		}

		payload["pairing_state"] = config.PairingStateClaimed
		payload["repair_code"] = code
		payload["repair_expires_at"] = expiresAt.Format(time.RFC3339)
	default:
		writeError(w, http.StatusConflict, "pairing_error", "pairing needs attention")
		return
	}

	s.logger.InfoContext(r.Context(), "local pairing code issued", "pairing_state", payload["pairing_state"])
	writeOK(w, http.StatusOK, payload)
}
```

4. After `sourceIP`, add:

```go
func fromLoopback(r *http.Request) bool {
	address, err := netip.ParseAddr(sourceIP(r))

	return err == nil && address.Unmap().IsLoopback()
}
```

- [ ] **Step 4: Verify**

Run: `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go vet ./...`
Expected: no output, exit 0.
If Docker is running: `docker build --target test .` — `TestServer_PairLocal` and the existing pairing tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/api/api.go internal/api/api_test.go
git commit -m "feat(api): hand claim and repair codes to callers on the intercom itself" -m "POST /api/v3/pair/local answers loopback peers only, so Home Assistant can pair over SSH; on a companion with no owner it also sets the web UI owner." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: `install.sh` records its firewall ports; `uninstall.sh`; script tests; docs

**Files:**
- Modify: `scripts/install.sh` (top constants; `ensure_persistent_firewall_port_value`)
- Create: `scripts/uninstall.sh`
- Create: `scripts/tests/run.sh`
- Modify: `README.md` (section "Uninstall"), `docs/build-and-install.md` (§0 quick reference, new §3.5)

**Interfaces:**
- Produces: `/home/bticino/cfg/extra/companion/firewall-added`, one TCP port per line, written by `install.sh` for each port it adds to the `# ssh & sip enabled` list; read by `uninstall.sh`.

- [ ] **Step 1: Write the failing tests**

Create `scripts/tests/run.sh`:

```sh
#!/usr/bin/env sh
# Logic tests for install.sh and uninstall.sh. Each case runs a copy of the
# script with its absolute paths moved under a scratch root and with fake id,
# mount and init script, so it runs unprivileged under any POSIX sh:
#   sh scripts/tests/run.sh
set -u

HERE="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
SCRIPTS="$(dirname "${HERE}")"
PASSED=0
FAILED=0

export COMPANION_SIP_INBOUND=0 COMPANION_HEALTHCHECK_TIMEOUT_SEC=1

check() {
	description="$1"
	shift
	if "$@"; then
		PASSED=$((PASSED + 1))
	else
		FAILED=$((FAILED + 1))
		printf 'not ok - %s\n' "${description}"
		sed 's/^/    | /' "${R}/out" 2>/dev/null || true
	fi
}

setup() {
	R="$(mktemp -d)"
	B="${R}/home/bticino/cfg/extra/companion"
	mkdir -p "${R}/shim" "${R}/var/run" "${R}/etc/init.d" "${R}/etc/network/if-pre-up.d" "${R}/etc/flexisip/users" "${B}"
	for n in 0 1 2 3 4 5 6; do mkdir -p "${R}/etc/rc${n}.d"; done
	sed -e "s|/home/bticino|${R}/home/bticino|g" -e "s|/var/run|${R}/var/run|g" -e "s|/etc/|${R}/etc/|g" \
		-e "s|/tmp/companion-install|${R}/dl|g" "${SCRIPTS}/$1" > "${R}/run.sh"
	printf '#!/bin/sh\necho "${FAKE_UID:-0}"\n' > "${R}/shim/id"
	printf '#!/bin/sh\nexit 0\n' > "${R}/shim/mount"
	chmod +x "${R}/shim/id" "${R}/shim/mount"
}

run_script() {
	PATH="${R}/shim:${PATH}" sh "${R}/run.sh" "$@" > "${R}/out" 2>&1
	echo $? > "${R}/rc"
}

rc_is() { [ "$(cat "${R}/rc")" = "$1" ]; }
out_has() { grep -qF -- "$1" "${R}/out"; }
# file_is <file> <printf format>: the file holds exactly what the format prints.
file_is() { printf "$2" | cmp -s - "$1"; }

# The hook as the stock firmware ships it, with 80 already open. Written with
# printf rather than a heredoc because the loop body is tab-indented.
STOCK_HOOK='# ssh & sip enabled\nfor i in 22 80 5060; do\n\tiptables -A INPUT -p tcp --dport $i -j ACCEPT\ndone\n\n#disable all other stuff\niptables -P INPUT DROP\n'

# The same hook after install.sh added its TCP ports and UDP blocks.
INSTALLED_HOOK='# ssh & sip enabled\nfor i in 22 80 5060 8080 8554 51826; do\n\tiptables -A INPUT -p tcp --dport $i -j ACCEPT\ndone\n\n# companion udp service\niptables -A INPUT -p udp -m udp --dport 5353 -j ACCEPT\n\n# companion udp service\niptables -A INPUT -p udp -m udp --dport 8555 -j ACCEPT\n\n#disable all other stuff\niptables -P INPUT DROP\n'

write_hook() {
	printf "${STOCK_HOOK}" > "${R}/etc/network/if-pre-up.d/iptables"
}

# A companion as install.sh leaves it, with 80 already open before it came.
write_installed_companion() {
	printf '#!/bin/sh\necho "init $1" >> "%s/init.log"\nexit 0\n' "${R}" > "${R}/etc/init.d/companion"
	chmod +x "${R}/etc/init.d/companion"
	for n in 2 3 4 5; do ln -s ../init.d/companion "${R}/etc/rc${n}.d/S99zzcompanion"; done
	for n in 0 1 6; do ln -s ../init.d/companion "${R}/etc/rc${n}.d/K55companion"; done
	echo binary > "${B}/companion"
	printf '8080\n8554\n51826\n' > "${B}/firewall-added"
	printf "${INSTALLED_HOOK}" > "${R}/etc/network/if-pre-up.d/iptables"
	printf 'user1@example.local abc ;\ncompanion@example.local abc ;\n' > "${R}/etc/flexisip/users/users.db.txt"
	printf '<sip:alluser@example.local> <sip:user1@example.local>, <sip:companion@example.local>\n' > "${R}/etc/flexisip/users/route.conf"
	touch "${R}/etc/flexisip/users/users.db.txt.companion.bak" "${R}/etc/flexisip/users/route.conf.companion.bak"
}

test_install_records_the_ports_it_adds() {
	setup install.sh
	mkdir -p "${R}/init.d"
	printf '#!/bin/sh\nexit 0\n' > "${R}/init.d/companion"
	printf '#!/bin/sh\n[ "${1:-}" = -version ] && echo v1.2.3\nexit 0\n' > "${R}/companion"
	chmod +x "${R}/init.d/companion" "${R}/companion"
	write_hook
	run_script "${R}/companion"
	check "install records exactly the TCP ports it added" file_is "${B}/firewall-added" '8080\n8554\n51826\n'
}

test_uninstall_removes_the_companion() {
	setup uninstall.sh
	write_installed_companion
	run_script
	check "uninstall exits 0" rc_is 0
	check "uninstall ends with the OK line" out_has "OK: Companion removed. Restart Flexisip or reboot the intercom."
	check "uninstall stops the service" grep -qx "init stop" "${R}/init.log"
	check "uninstall removes the init script" [ ! -e "${R}/etc/init.d/companion" ]
	check "uninstall removes the boot links" [ ! -e "${R}/etc/rc5.d/S99zzcompanion" ] && [ ! -e "${R}/etc/rc6.d/K55companion" ]
	check "uninstall removes only the recorded TCP ports and the UDP blocks" file_is "${R}/etc/network/if-pre-up.d/iptables" "${STOCK_HOOK}"
	check "uninstall removes the SIP user" file_is "${R}/etc/flexisip/users/users.db.txt" 'user1@example.local abc ;\n'
	check "uninstall removes the alluser contact" file_is "${R}/etc/flexisip/users/route.conf" '<sip:alluser@example.local> <sip:user1@example.local>\n'
	check "uninstall deletes the backups" [ ! -e "${R}/etc/flexisip/users/route.conf.companion.bak" ]
	check "uninstall deletes the companion directory" [ ! -e "${B}" ]
	check "uninstall releases the lock" [ ! -e "${R}/var/run/companion-install.lock" ]
}

test_uninstall_without_port_record_keeps_tcp_ports() {
	setup uninstall.sh
	write_installed_companion
	rm -f "${B}/firewall-added"
	run_script
	check "uninstall without a port record exits 0" rc_is 0
	check "uninstall without a port record warns" out_has "WARN: No record of the TCP ports the installer added"
	check "uninstall without a port record keeps the TCP list" grep -q "for i in 22 80 5060 8080 8554 51826; do" "${R}/etc/network/if-pre-up.d/iptables"
}

test_uninstall_leaves_an_unexpected_hook_alone() {
	setup uninstall.sh
	write_installed_companion
	printf 'iptables -P INPUT ACCEPT\n' > "${R}/etc/network/if-pre-up.d/iptables"
	run_script
	check "uninstall with an unexpected hook exits 0" rc_is 0
	check "uninstall with an unexpected hook warns" out_has "WARN: Could not find the SSH/SIP firewall block"
	check "uninstall with an unexpected hook leaves it unchanged" file_is "${R}/etc/network/if-pre-up.d/iptables" 'iptables -P INPUT ACCEPT\n'
}

test_uninstall_with_nothing_installed() {
	setup uninstall.sh
	rm -rf "${B}" "${R}/etc/flexisip/users"
	run_script
	check "uninstall with nothing installed exits 0" rc_is 0
	check "uninstall with nothing installed ends with the OK line" out_has "OK: Companion removed."
}

test_uninstall_refuses_a_second_run() {
	setup uninstall.sh
	write_installed_companion
	sleep 30 &
	holder=$!
	mkdir -p "${R}/var/run/companion-install.lock"
	echo "${holder}" > "${R}/var/run/companion-install.lock/pid"
	run_script
	kill "${holder}" 2>/dev/null
	check "uninstall during an installation exits 1" rc_is 1
	check "uninstall during an installation says why" out_has "FAIL: Another installation or removal is already running"
	check "uninstall during an installation changes nothing" [ -e "${R}/etc/init.d/companion" ]
}

test_uninstall_requires_root() {
	setup uninstall.sh
	FAKE_UID=1000
	export FAKE_UID
	run_script
	unset FAKE_UID
	check "uninstall as non-root exits 1" rc_is 1
	check "uninstall as non-root says why" out_has "FAIL: This script must run as root."
}

test_install_records_the_ports_it_adds
test_uninstall_removes_the_companion
test_uninstall_without_port_record_keeps_tcp_ports
test_uninstall_leaves_an_unexpected_hook_alone
test_uninstall_with_nothing_installed
test_uninstall_refuses_a_second_run
test_uninstall_requires_root

printf '%s passed, %s failed\n' "${PASSED}" "${FAILED}"
[ "${FAILED}" -eq 0 ]
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `sh scripts/tests/run.sh`
Expected: every check fails (`uninstall.sh` does not exist; `firewall-added` is not written), last line `0 passed, 23 failed` or similar, exit 1.

- [ ] **Step 3: Record the ports in `install.sh`**

In `scripts/install.sh`, after `LOCK_DIR="/var/run/${SERVICE_NAME}-install.lock"` add:

```sh
# Ports such as 80 may be in the firewall list before the companion arrives,
# so uninstall.sh removes only the ones listed here.
FIREWALL_ADDED="${BASE_DIR}/firewall-added"
```

In `ensure_persistent_firewall_port_value`, replace the line

```sh
		cp "${tmp}" "${hook}"; rm -f "${tmp}"; log "Persisted companion firewall port ${port} in ${hook}."; return
```

with

```sh
		cp "${tmp}" "${hook}"; rm -f "${tmp}"; printf '%s\n' "${port}" >> "${FIREWALL_ADDED}"; log "Persisted companion firewall port ${port} in ${hook}."; return
```

- [ ] **Step 4: Write `scripts/uninstall.sh`**

```sh
#!/usr/bin/env sh
set -eu

# Removes what install.sh put on the intercom. It is fetched and run on its own,
# like the installer, so it carries its own copy of the installer's helpers.

SERVICE_NAME="companion"
BASE_DIR="/home/bticino/cfg/extra/companion"
INIT_SCRIPT="/etc/init.d/${SERVICE_NAME}"
FIREWALL_HOOK="/etc/network/if-pre-up.d/iptables"
FIREWALL_ADDED="${BASE_DIR}/firewall-added"
COMPANION_TCP_PORTS="8080 80 8554 51826"
FLEXISIP_USERS_DB="/etc/flexisip/users/users.db.txt"
FLEXISIP_ROUTE="/etc/flexisip/users/route.conf"
SIP_USER="companion"
# The installer's lock, so that a removal cannot run during an installation.
LOCK_DIR="/var/run/${SERVICE_NAME}-install.lock"

ROOT_WAS_REMOUNTED=0
LOCK_HELD=0
FAILURES=0
CURRENT_STEP="starting"
ABORT_REPORTED=0
RUN_FINISHED=0

# Writes are best-effort and the run carries on if the session that started it
# goes away, as in install.sh.
log() { printf 'INFO: %s\n' "$*" 2>/dev/null || true; }
ok() { printf 'OK: %s\n' "$*" 2>/dev/null || true; }
warn() { printf 'WARN: %s\n' "$*" 2>/dev/null || true; }
fail() { printf 'FAIL: %s\n' "$*" 2>/dev/null || true; FAILURES=$((FAILURES + 1)); }

die() {
	printf 'FAIL: %s\n' "$*" >&2 2>/dev/null || true
	ABORT_REPORTED=1
	exit 1
}

restore_root_ro() {
	if [ "${ROOT_WAS_REMOUNTED}" -eq 1 ]; then
		mount -o remount,ro / || true
		ROOT_WAS_REMOUNTED=0
		log "Restored / as read-only."
	fi
}

remount_root_rw() {
	if [ "${ROOT_WAS_REMOUNTED}" -eq 0 ]; then
		mount -o remount,rw /
		ROOT_WAS_REMOUNTED=1
		log "Remounted / as read-write."
	fi
}

acquire_install_lock() {
	if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
		holder="$(cat "${LOCK_DIR}/pid" 2>/dev/null || true)"
		if [ -n "${holder}" ] && [ -d "/proc/${holder}" ]; then
			die "Another installation or removal is already running (pid ${holder})."
		fi
		rm -rf "${LOCK_DIR}"
		mkdir "${LOCK_DIR}" || die "Could not create the lock ${LOCK_DIR}."
	fi
	echo "$$" > "${LOCK_DIR}/pid"
	LOCK_HELD=1
}

release_install_lock() {
	[ "${LOCK_HELD}" -eq 1 ] || return 0
	rm -rf "${LOCK_DIR}"
	LOCK_HELD=0
}

on_exit() {
	rc=$?
	set +e
	restore_root_ro
	release_install_lock
	if [ "${rc}" -ne 0 ] && [ "${RUN_FINISHED}" -eq 0 ] && [ "${ABORT_REPORTED}" -eq 0 ]; then
		printf 'FAIL: Removal aborted while %s (exit %s).\n' "${CURRENT_STEP}" "${rc}" >&2 2>/dev/null
	fi
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap '' HUP PIPE

require_root() {
	[ "$(id -u)" -eq 0 ] || die "This script must run as root."
}

stop_service() {
	if [ ! -x "${INIT_SCRIPT}" ]; then
		log "No init script at ${INIT_SCRIPT}; nothing to stop."
		return 0
	fi
	if "${INIT_SCRIPT}" stop; then ok "Stopped ${SERVICE_NAME}."; else fail "Could not stop ${SERVICE_NAME}."; fi
}

remove_service_files() {
	before="${FAILURES}"
	for runlevel in 2 3 4 5; do rm -f "/etc/rc${runlevel}.d/S99zz${SERVICE_NAME}" || fail "Could not remove /etc/rc${runlevel}.d/S99zz${SERVICE_NAME}."; done
	for runlevel in 0 1 6; do rm -f "/etc/rc${runlevel}.d/K55${SERVICE_NAME}" || fail "Could not remove /etc/rc${runlevel}.d/K55${SERVICE_NAME}."; done
	rm -f "${INIT_SCRIPT}" || fail "Could not remove ${INIT_SCRIPT}."
	[ "${FAILURES}" -ne "${before}" ] || ok "Removed the init script and its boot links."
}

remove_firewall_tcp_ports() {
	if [ ! -f "${FIREWALL_ADDED}" ]; then
		warn "No record of the TCP ports the installer added; ports ${COMPANION_TCP_PORTS} may still be open in ${FIREWALL_HOOK}."
		return 0
	fi
	ports="$(tr '\n' ' ' < "${FIREWALL_ADDED}")"
	ports="${ports% }"
	tmp="${FIREWALL_HOOK}.tmp.$$"
	rc=0
	awk -v ports="${ports}" '
		BEGIN { n = split(ports, p, /[[:space:]]+/); for (i = 1; i <= n; i++) if (p[i] != "") drop[p[i]] = 1 }
		/# ssh \& sip enabled/ { inblock = 1; print; next }
		inblock && /^[[:space:]]*for i in .*; do[[:space:]]*$/ {
			lead = $0; sub(/for i in.*/, "", lead)
			line = $0; sub(/^[[:space:]]*for i in[[:space:]]*/, "", line); sub(/[[:space:]]*; do[[:space:]]*$/, "", line)
			m = split(line, a, /[[:space:]]+/); out = lead "for i in"; kept = 0
			for (i = 1; i <= m; i++) if (a[i] != "" && !(a[i] in drop)) { out = out " " a[i]; kept++ }
			if (kept == 0) { emptied = 1; exit }
			print out "; do"; patched = 1; inblock = 0; next
		}
		{ print }
		END { if (emptied) exit 43; if (!patched) exit 42 }
	' "${FIREWALL_HOOK}" > "${tmp}" || rc=$?
	case "${rc}" in
	0) if cp "${tmp}" "${FIREWALL_HOOK}"; then ok "Removed TCP ports ${ports} from ${FIREWALL_HOOK}."; else fail "Could not write ${FIREWALL_HOOK}."; fi ;;
	42) warn "Could not find the SSH/SIP firewall block in ${FIREWALL_HOOK}; TCP ports left unchanged." ;;
	43) warn "Removing ports ${ports} would empty the SSH/SIP port list in ${FIREWALL_HOOK}; left unchanged." ;;
	*) fail "Could not edit ${FIREWALL_HOOK}." ;;
	esac
	rm -f "${tmp}"
}

remove_firewall_udp_blocks() {
	if ! grep -q '^# companion udp service$' "${FIREWALL_HOOK}"; then
		log "No companion UDP rules in ${FIREWALL_HOOK}."
		return 0
	fi
	tmp="${FIREWALL_HOOK}.tmp.$$"
	# Each block is the marker, its iptables line and the blank line install.sh
	# puts after it.
	if awk '
		/^# companion udp service$/ { skip = 1; next }
		skip == 1 { skip = 2; next }
		skip == 2 { skip = 0; if ($0 ~ /^[[:space:]]*$/) next }
		{ print }
	' "${FIREWALL_HOOK}" > "${tmp}" && cp "${tmp}" "${FIREWALL_HOOK}"; then
		ok "Removed the companion UDP rules from ${FIREWALL_HOOK}."
	else
		fail "Could not remove the companion UDP rules from ${FIREWALL_HOOK}."
	fi
	rm -f "${tmp}"
}

remove_firewall_entries() {
	if [ ! -f "${FIREWALL_HOOK}" ]; then
		log "${FIREWALL_HOOK} not found; no firewall entries to remove."
		return 0
	fi
	remove_firewall_tcp_ports
	remove_firewall_udp_blocks
}

remove_sip_user() {
	if [ ! -f "${FLEXISIP_USERS_DB}" ] || [ ! -f "${FLEXISIP_ROUTE}" ]; then
		log "Flexisip user files not found; no SIP user to remove."
		return 0
	fi
	before="${FAILURES}"

	if grep -q "^${SIP_USER}@" "${FLEXISIP_USERS_DB}"; then
		tmp="${FLEXISIP_USERS_DB}.companion.tmp"
		if awk -v user="${SIP_USER}@" 'index($0, user) != 1' "${FLEXISIP_USERS_DB}" > "${tmp}" && cp "${tmp}" "${FLEXISIP_USERS_DB}"; then
			log "Removed ${SIP_USER} from ${FLEXISIP_USERS_DB}."
		else
			fail "Could not remove ${SIP_USER} from ${FLEXISIP_USERS_DB}."
		fi
		rm -f "${tmp}"
	fi

	if grep -q "sip:${SIP_USER}@" "${FLEXISIP_ROUTE}"; then
		tmp="${FLEXISIP_ROUTE}.companion.tmp"
		sed "s|, <sip:${SIP_USER}@[^>]*>||g" "${FLEXISIP_ROUTE}" > "${tmp}"
		# route.conf must stay one line per group: a mangled file breaks every
		# call on the intercom, so the result is checked before it is used.
		groups_before="$(grep -c 'sip:alluser@' "${FLEXISIP_ROUTE}" || true)"
		groups_after="$(grep -c 'sip:alluser@' "${tmp}" || true)"
		if [ "${groups_before}" = "${groups_after}" ] && ! grep -q "sip:${SIP_USER}@" "${tmp}" && cp "${tmp}" "${FLEXISIP_ROUTE}"; then
			log "Removed ${SIP_USER} from the alluser group."
		else
			fail "route.conf rewrite failed validation; original left untouched."
		fi
		rm -f "${tmp}"
	fi

	# The backups predate anything BTicino changed since the installation, so
	# restoring them could undo it; they are dropped instead.
	rm -f "${FLEXISIP_USERS_DB}.companion.bak" "${FLEXISIP_ROUTE}.companion.bak"
	[ "${FAILURES}" -ne "${before}" ] || ok "Removed the ${SIP_USER} SIP user."
}

remove_files() {
	if rm -rf "${BASE_DIR}"; then ok "Removed ${BASE_DIR}."; else fail "Could not remove ${BASE_DIR}."; fi
}

main() {
	require_root
	acquire_install_lock
	log "Starting companion removal"

	CURRENT_STEP="stopping the service"
	stop_service
	CURRENT_STEP="remounting / read-write"
	remount_root_rw
	CURRENT_STEP="removing the init service"
	remove_service_files
	CURRENT_STEP="removing the firewall entries"
	remove_firewall_entries
	CURRENT_STEP="removing the SIP user"
	remove_sip_user
	restore_root_ro
	CURRENT_STEP="removing ${BASE_DIR}"
	remove_files

	RUN_FINISHED=1
	[ "${FAILURES}" -eq 0 ] || { log "Removal finished with ${FAILURES} failure(s)."; exit 1; }
	ok "Companion removed. Restart Flexisip or reboot the intercom."
}

main "$@"
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `sh scripts/tests/run.sh`
Expected: no `not ok` lines, last line `<N> passed, 0 failed`, exit 0.

- [ ] **Step 6: Update the docs**

In `README.md`, replace the body of `## Uninstall` (from the first code block to the paragraph about firewall entries and `.companion.bak`, inclusive) with:

````markdown
Connect to the intercom over SSH and run the uninstaller:

```sh
wget -qO /tmp/uninstall.sh "https://github.com/abeilprincipino/BTicino-GO-Companion/raw/main/scripts/uninstall.sh" && sh /tmp/uninstall.sh
```

It stops the service, removes the init script and its boot links, removes the companion's firewall
rules from `/etc/network/if-pre-up.d/iptables`, removes the `companion` SIP user from Flexisip, and
deletes `/home/bticino/cfg/extra/companion`, configuration and pairing included. Only the TCP ports
the installer added are closed: installations made before the installer recorded them keep their TCP
ports, and the uninstaller says so with a `WARN:` line. It ends with
`OK: Companion removed. Restart Flexisip or reboot the intercom.` and exits with code `1` if any step
failed.
````

In `docs/build-and-install.md`, add after §3.4 a section:

````markdown
### 3.5 Disinstallazione

```sh
ssh root@<IP> 'wget -qO /tmp/uninstall.sh "https://github.com/abeilprincipino/BTicino-GO-Companion/raw/main/scripts/uninstall.sh" && sh /tmp/uninstall.sh'
```

Ferma il servizio, rimuove init script e symlink di boot, le regole firewall del companion, l'utente
SIP `companion` da Flexisip e la cartella `/home/bticino/cfg/extra/companion` (configurazione e
pairing compresi). Delle porte TCP chiude solo quelle che l'installer ha aggiunto e annotato in
`firewall-added`; sulle installazioni precedenti le lascia aperte con un `WARN:`. Non ripristina i
`.companion.bak` di Flexisip: annullerebbero le modifiche fatte da BTicino dopo l'installazione.
Usa lo stesso lock dell'installer. Poi va riavviato Flexisip, o il citofono.

I test di logica di `install.sh` e `uninstall.sh` girano senza root su qualsiasi `sh` POSIX:

```sh
sh scripts/tests/run.sh
```
````

- [ ] **Step 7: Commit**

```bash
git add scripts/install.sh scripts/uninstall.sh scripts/tests/run.sh README.md docs/build-and-install.md
git commit -m "feat(scripts): add an uninstaller and record the firewall ports the installer opens" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

## Out of this plan

The Home Assistant side (installer module, config flow, reconfigure) is a separate plan in
`BTicinoGO-Integration`: `docs/superpowers/plans/2026-10-22-ha-driven-install-integration.md`.
Running the uninstaller and the new endpoint on the real intercom is part of that plan's end-to-end
check.

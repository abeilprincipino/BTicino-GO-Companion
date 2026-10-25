# Install the Companion from Home Assistant — Design

**Date:** 2026-10-22
**Status:** Approved for planning
**Repos:** `BTicinoGO` (companion, branch `feat/ha-driven-install`, on top of `fix/installer-hardening`),
`BTicinoGO-Integration` (integration, a new branch from `v3`)

---

## 1. Goal

Let a user install the companion on a rooted intercom from the Home Assistant integration, without
opening a terminal: they add the integration, give the intercom's IP and its SSH user and password,
and the integration installs the companion, pairs with it and offers to reboot the intercom. The
same path reinstalls an existing companion.

## 2. Decisions

| Topic | Decision |
|---|---|
| Who downloads the release | The intercom, with the existing `install.sh`. |
| SSH password | Used for the length of the flow only, never stored. |
| Updates | Stay with the companion's own updater. Anything it cannot update is fixed by reinstalling. |
| Pairing | Done by Home Assistant over SSH, without the web UI owner setup and without typing a claim code. |
| Web UI | Home Assistant asks for the web UI owner username and password when the companion has none. |
| How the companion accepts SSH pairing | A loopback-only API endpoint (section 4.1). |
| Flexisip | Home Assistant offers to reboot the intercom at the end. |
| Uninstall | A `scripts/uninstall.sh`, not offered by Home Assistant. |

## 3. What exists today

- `install.sh` is hardened by `fix/installer-hardening`: every abort prints a `FAIL:` line, a dropped
  session does not stop it, a lock refuses a second run, a successful run prints
  `INFO: Installed version: <v>` before `Installation complete.`, and the exit code is meaningful.
- Pairing states (`internal/config/config.go`): `setup_required` → `claimable` → `claimed`, plus
  `error`. The claim code is derived from `WebUI.SessionSecret` (`deriveClaimCode`,
  `internal/auth/store.go`).
- `setup_required` → `claimable` happens only in the web UI owner setup
  (`handlePassword`, `internal/webui/server.go`): the first login with the default credentials
  `companion`/`companion` forces a new username and password, which sets `AdminUsername`,
  `AdminPasswordHash`, a new `SessionSecret`, then calls `auth.Store.StartInitialClaim`.
- Until that setup is done, anyone on the LAN can log in to the web UI with the default credentials.
  The web UI owner can issue a repair code, which `POST /api/v3/auth/recover` turns into a bearer
  token. **Leaving the web UI in bootstrap after pairing would hand the door to anyone on the LAN.**
  This is why Home Assistant must set the owner (section 2).
- The API server has no reverse proxy in front of it: `r.RemoteAddr` is the real peer.
- The integration's config flow (`config_flow.py`) knows `user` (URL), `zeroconf`, `claim`,
  `recover` and `reauth`. `setup_required` aborts with `setup_required`.
- `POST /api/v3/system/reboot` exists and is protected by the bearer token
  (`CompanionApiClient.async_reboot`).

## 4. Companion

### 4.1 `POST /api/v3/pair/local`

Unprotected route, registered next to `pair/challenge` and `pair/claim`.

**Loopback only.** The handler parses the host of `r.RemoteAddr`; unless it is a loopback address it
answers exactly like an unknown route (`s.notFound`), so the endpoint is not discoverable from the
LAN.

**Request** (JSON body, required like every JSON route; `{}` when there is no owner to send):

```json
{"owner": {"username": "...", "password": "..."}}
```

**Behaviour by pairing state:**

| State | Effect | Status | Payload (besides `ok`) |
|---|---|---|---|
| `setup_required` | Requires a valid `owner`. Sets the web UI owner and a new session secret and moves to `claimable`, in one config update. | 200 | `pairing_state: "claimable"`, `claim_code` |
| `claimable` | `owner` is ignored: an owner already exists. | 200 | `pairing_state: "claimable"`, `claim_code` |
| `claimed` | Issues a repair code (`IssueRepairCode`, 10 min lifetime). | 200 | `pairing_state: "claimed"`, `repair_code`, `repair_expires_at` (RFC 3339) |
| `setup_required` without a valid `owner` | none | 400 | error `invalid_owner`, with the rule that failed |
| `error` | none | 409 | error `pairing_error` |

Every 200 response also carries `device_id` and `instance_id`, so the caller can check that the
companion it reached over SSH is the one it reaches over HTTP.

The claim and the recovery themselves still go through the existing remote endpoints
(`pair/challenge` + `pair/claim`, `auth/recover`), so their rate limiting and source-IP checks are
unchanged.

### 4.2 One owner-setup rule, used twice

The owner rules now in `handlePassword` (username not empty, password of at least 8 characters,
neither equal to the defaults) and the owner write (bcrypt hash, new session secret) move to
`internal/auth`:

- `auth.ValidateOwner(username, password string) error`, returning an error that names the rule.
- `(*auth.Store).SetupOwner(username, password string) (claimCode string, err error)`: validates, then
  in a single `config.Update` sets `AdminUsername`, `AdminPasswordHash`, `SessionSecret` and
  `PairingState = claimable`, and returns the claim code. It fails with `ErrClaimNotAllowed` unless
  the state is `setup_required`.

`handlePassword` calls `SetupOwner` on its bootstrap path when the companion is
`setup_required`, instead of writing the config and then calling `StartInitialClaim`. A bootstrap
on a companion that is already `claimable` or `claimed` (the web UI owner was never set, which the
web UI tests rely on) and the non-bootstrap path (password change) keep writing the owner as today,
without touching the pairing state. `StartInitialClaim` stays: the test helpers use it to reach
`claimable` without setting an owner. The defaults `companion`/`companion` move with the rules. Bootstrap web UI sessions opened before a
local setup become invalid on their own: they are only valid while `AdminPasswordHash` is empty.

### 4.3 `scripts/uninstall.sh`

Same conventions as `install.sh`: `set -eu`, `INFO/OK/WARN/FAIL` lines, `die`, the abort report, the
lock (the same lock directory, so an uninstall cannot run during an install), `HUP`/`PIPE` ignored,
`/` remounted read-write only while needed and restored read-only on exit.

In order:

1. Stops the service (`/etc/init.d/companion stop`) if the init script exists.
2. Removes `/etc/init.d/companion`, `S45companion` and `S99zzcompanion` from `rc{2,3,4,5}.d` and
   `K55companion` from `rc{0,1,6}.d`.
3. Removes the companion's ports from `/etc/network/if-pre-up.d/iptables`:
   - the `# companion udp service` blocks, which the installer marks;
   - from the `for i in ...; do` line of the `# ssh & sip enabled` block, only the TCP ports the
     installer added. A port such as `80` may have been in that list before the companion, so the
     installer now records each TCP port it adds, one per line, in
     `/home/bticino/cfg/extra/companion/firewall-added`. Without that file (installations made
     before this change) the TCP ports are left in place with a `WARN:` naming them.

   Other ports and lines are left untouched; if the file does not have the expected shape it prints
   a `WARN:` and leaves it as is.
4. Removes `companion@<domain>` from `users.db.txt` and `, <sip:companion@<domain>>` from the
   `alluser` line of `route.conf`, with the same one-line-per-group validation as the installer
   before replacing the file. The `.companion.bak` copies are not restored: they would undo
   anything BTicino changed after the installation. They are deleted.
5. Deletes `/home/bticino/cfg/extra/companion`, including `config.yaml`: the pairing is gone.
6. Ends with `OK: Companion removed. Restart Flexisip or reboot the intercom.` and exits `0`.

Only the preconditions (root, lock) use `die`. Steps 1–5 report a failure with `fail` and carry on,
so one failed step does not leave the rest in place; the script then exits `1` instead of printing
the final `OK:` line.

It is run like the installer: `wget` of the raw script from GitHub, then `sh`. The README and
`docs/build-and-install.md` replace their manual uninstall steps with it.

## 5. Home Assistant

### 5.1 Entry point

The manual flow (`async_step_user`) becomes a menu:

- **Connect to an installed companion** — the current URL form, unchanged behaviour, moved to a
  `connect` step.
- **Install on the intercom** — section 5.2.

Zeroconf discovery is unchanged: it only finds companions that are already installed.

### 5.2 Install flow

| Step | Kind | What it does |
|---|---|---|
| `install` | form | Host (IP or name), SSH username (default `root`), SSH password. Connects over SSH; on failure shows `cannot_connect` or `invalid_auth` on the form. |
| `install_run` | progress | Starts the installer and follows it (section 5.4). |
| `install_failed` | form | Shows the installer's `FAIL:` lines and offers to retry (back to `install_run` with the same credentials). |
| `owner` | form | Only when `/api/v3/auth/status` says `setup_required`: web UI username and password, with the rules of 4.2 checked locally before sending. A 400 `invalid_owner` comes back on this form. |
| — | internal | `pair/local` over SSH (section 5.5), then claim or recover with the existing code paths. |
| `reboot` | menu | "Reboot the intercom now (needed to receive calls)" / "Later". Now: `async_reboot` with the new token. Then the entry is created. |

The entry stores what it stores today plus `ssh_username` and `ssh_host_key` (section 5.3). The
companion URL is `http://<host>:8080`.

### 5.3 SSH

- Library: `asyncssh`, pinned in the manifest's `requirements`.
- **Host key:** accepted on first use and stored in the entry as `ssh_host_key` (the public key in
  OpenSSH format). The reconfigure flow (5.6) requires the same key; a different key is an error on
  the form, with a checkbox "The intercom was reflashed: accept its new key" to go on.
- All SSH work lives in a new module `installer.py`, behind a small interface used by the config
  flow:

  ```python
  class IntercomShell:
      async def async_connect(host, username, password, expected_host_key=None) -> str  # host key
      async def async_start_installer() -> None
      async def async_read_progress() -> InstallerProgress  # log lines, finished, exit code
      async def async_pair_local(owner: tuple[str, str] | None) -> dict
      async def async_close() -> None
  ```

  Errors: `ShellConnectError`, `ShellAuthError`, `ShellHostKeyMismatch`, `PairLocalError` (with the
  HTTP status and error code). `config_flow.py` never imports `asyncssh`.

### 5.4 Running the installer

`async_start_installer` runs, in one SSH command:

```sh
rm -f /tmp/companion-install.log /tmp/companion-install.rc
wget -qO /tmp/companion-install.sh <INSTALL_SCRIPT_URL>
setsid sh -c 'sh /tmp/companion-install.sh >/tmp/companion-install.log 2>&1; echo $? >/tmp/companion-install.rc' \
    </dev/null >/dev/null 2>&1 &
```

`INSTALL_SCRIPT_URL` is a constant: the raw `scripts/install.sh` on the companion fork's `main`.
A failed `wget` is reported as an installer failure with its own message.

`async_read_progress` reads both files every 2 s. The progress fraction comes from known installer
lines:

| Line contains | Fraction |
|---|---|
| `Downloading latest release bundle` | 0.1 |
| `Installed binary` | 0.4 |
| `Installed gst runtime` | 0.5 |
| `Registered init service` | 0.6 |
| `Provisioning SIP inbound` | 0.75 |
| `Waiting for health endpoint` | 0.9 |
| the `.rc` file exists | 1.0 |

- Exit code `0`: the flow goes on and keeps `Installed version:` for the log.
- Any other code: `install_failed` with every `FAIL:` line, or the last 10 lines when there is none.
- No `.rc` after 10 minutes: `install_failed` with a timeout message.
- A dropped SSH connection while polling is reopened with the credentials held by the flow; three
  failed reconnections end in `install_failed`.

Because the installer is detached and survives a dropped session, a Home Assistant restart during
`install_run` loses the flow but not the installation. The user starts the install flow again: the
second run reinstalls the same release and then pairs.

### 5.5 Pairing over SSH

1. `GET /api/v3/auth/status` over HTTP: `device_id`, `instance_id`, `pairing_state`.
2. `setup_required` → step `owner`.
3. `async_pair_local` runs `curl` against `http://127.0.0.1:8080/api/v3/pair/local` on the intercom,
   with the JSON on stdin (`--data-binary @-`), so the web UI password is never on a command line.
   It returns the HTTP status and the body.
4. `device_id` and `instance_id` from `pair/local` must equal those from step 1, otherwise the flow
   aborts with `wrong_device`.
5. `claim_code` → `async_pair_challenge` + `async_pair_claim`; `repair_code` →
   `async_recover_bearer`. The existing `_async_authorize` is reused.

### 5.6 Reinstall: `async_step_reconfigure`

Form with host and SSH username prefilled from the entry and an empty password. Then
`install_run`, `pair_local` (the companion is `claimed`, so a repair code comes back), recover, and
`async_update_reload_and_abort` with the new token, the host key and the URL. The `reboot` menu is
shown before the update. Because it recovers the token, reinstalling also fixes an entry whose
token was lost.

### 5.7 Strings

All new steps, errors and menu options go in `translations/en.json`, like the existing ones.

## 6. Error handling summary

| Failure | Where it shows |
|---|---|
| SSH unreachable / refused | `install` form: `cannot_connect` |
| SSH wrong credentials | `install` form: `invalid_auth` |
| Host key changed | `reconfigure` form: `host_key_changed` + checkbox |
| Installer download, abort or failed checks | `install_failed` with the `FAIL:` lines, retry |
| Installer timeout | `install_failed`, timeout message, retry |
| Companion not reachable over HTTP after install | `install_failed`: `cannot_connect` |
| Invalid web UI owner | `owner` form: rule that failed |
| Different device on SSH and HTTP | abort `wrong_device` |
| Claim / recover rejected | abort `pairing_error` |
| Reboot request failed | logged as a warning; the entry is created anyway |

## 7. Testing

**Companion** (Go, run on Linux as the repo's other tests):

- `pair/local`: refused from a non-loopback address (404 shape), each pairing state of 4.1,
  `invalid_owner` per rule, `device_id`/`instance_id` present.
- `auth.SetupOwner` and `ValidateOwner`: each rule, refusal outside `setup_required`, the single
  update leaves the config valid (`config` validation of state vs token hash).
- Web UI bootstrap: the existing tests keep passing through `SetupOwner`.

**`uninstall.sh`:** logic tests with shims as for `install.sh` (lock, abort report, firewall and
Flexisip file edits on sample files, including a file with an unexpected shape and an installation
without `firewall-added`), then a run on the real intercom. `install.sh` gets a test that
`firewall-added` lists exactly the ports it added.

**Integration** (pytest, existing `tests/` setup):

- Config flow with `IntercomShell` mocked: menu, install happy path with and without `owner`,
  `install_failed` and retry, timeout, `wrong_device`, `reboot` now/later, reconfigure with the same
  key, a changed key, and the accept-new-key checkbox.
- `installer.py` with a fake `asyncssh` connection: progress parsing from sample logs, exit-code
  handling, reconnection, `pair_local` request and response parsing.

**End to end on the intercom:** fresh install, reinstall over a paired companion, Home Assistant
restarted during `install_run`, wrong password, host key change.

## 8. To verify on the real intercom before or during implementation

- The SSH server and the algorithms it offers: an old dropbear may need legacy key-exchange or
  host-key algorithms enabled explicitly in `asyncssh`.
- `setsid` is available (busybox normally has it); otherwise fall back to `nohup`.
- `curl` is at `/usr/bin/curl` (the init script already depends on it).

## 9. Out of scope

- Uninstalling from Home Assistant.
- Installing a local build from Home Assistant.
- Translations other than English.
- Changing the existing `connect` path's abort on `setup_required`.

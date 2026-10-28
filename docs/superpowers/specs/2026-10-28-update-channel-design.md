# Update Channel (Stable / Beta) — Design

**Date:** 2026-10-28
**Status:** Approved for planning
**Repo:** `BTicinoGO` (companion), branch `feat/update-channel`

---

## 1. Goal

Let the user choose, in the Web UI, whether the companion updates from the **Stable** channel or
the **Beta** channel. Stable keeps working exactly as today. Beta lists the GitHub pre-releases of
the companion and lets the user pick which one to install.

Decisions taken with the user:

| Question | Decision |
|---|---|
| Where the channel lives | Persisted in config: `system.updates.channel`, default `stable` |
| Which versions Beta lists | **Every** published pre-release, including older ones (downgrade between betas allowed); the installed one is marked |
| Home Assistant API and the 3 h background check | **Stay Stable**; Beta selection exists only in the Web UI |
| Release workflow | Tags with a suffix (`v0.9.0-beta.1`) are published as GitHub pre-releases |

## 2. What the code already gives

- `system.GitHubReleaseClient` fetches `/repos/<repo>/releases/latest` and downloads an asset.
  GitHub's `latest` endpoint never returns a pre-release or a draft.
- `system.Updater` (`internal/system/update.go`) has `Status`, `Check`, `Stage`, `Install`.
  `Stage` downloads `companion.tar.gz`, verifies its `sha256:` digest, extracts
  `companion/companion` and renames it to `<DataDir>/companion.new`. `Install` stages, then
  restarts the companion service after the response has gone out. `stageArchive` does not care
  about versions, so it can stage a downgrade as is.
- `newerVersion` already compares full semver, pre-release identifiers included
  (`v0.9.0` > `v0.9.0-beta.2` > `v0.9.0-beta.1`).
- The policy closure in `internal/app/run.go` reads `configStore.Snapshot()` on every call, so a
  config change reaches the updater without a restart.
- Web UI: `GET /webui/api/management/update`, `POST .../update/check`, `POST .../update`
  (install), and the Updates page in `internal/webui/static/{index.html,app.js}`.
- `.github/workflows/release.yaml` publishes every `v*` tag as a normal release.

## 3. Design

### 3.1 Release source (`internal/system/release.go`)

- `ReleaseManifest` gains `Prerelease bool` (`prerelease`), `Draft bool` (`draft`) and
  `PublishedAt time.Time` (`published_at`).
- `NewGitHubReleaseClient(client, repoURL)` now takes the repository API base
  (`https://api.github.com/repos/<owner>/<repo>`) and derives `<base>/releases/latest` and
  `<base>/releases?per_page=50`.
- New `ParseGitHubReleaseList(reader) ([]ReleaseManifest, error)`: decodes a JSON array. A body
  that is not an array is `ErrInvalidReleaseManifest`. Each entry is validated with the same rules
  as `ParseGitHubReleaseManifest` (tag, asset name, `sha256:` digest, https URL). **An invalid entry
  is skipped**, so one broken old release does not hide the others. Drafts are skipped.
- New `GitHubReleaseClient.List(ctx) ([]ReleaseManifest, error)`, with the same headers, status
  check and 1 MiB body limit as `Latest`.
- `ReleaseSource` gains `List(context.Context) ([]ReleaseManifest, error)`.

### 3.2 Updater (`internal/system/update.go`)

- `UpdatePolicy` gains `Channel string`; `UpdateStatus` gains `Channel string` (`channel`), filled
  from the policy (`stable` when empty).
- Constants `UpdateChannelStable = "stable"`, `UpdateChannelBeta = "beta"`.
- New type `Prerelease { Version string; PublishedAt time.Time; Installed bool }` (JSON
  `version`, `published_at`, `installed`).
- New `Updater.Prereleases(ctx) ([]Prerelease, error)`: `available()` must pass; calls
  `source.List`, keeps entries with `Prerelease == true` whose tag parses as semver, sorts them
  newest first by `newerVersion`, marks `Installed` when the tag equals the current version.
  Does not touch `latest`/`staged`/`err`.
- New `Updater.InstallVersion(ctx, version) (UpdateStatus, error)`:
  - `available()` must pass and `restart` must be set, otherwise `ErrUpdateUnavailable`;
  - re-fetches the list and looks the tag up **among the pre-releases only**; not found →
    `ErrReleaseNotFound` (new). A stable tag is therefore refused;
  - tag equal to the current version → `ErrReleaseAlreadyInstalled` (new);
  - otherwise downloads the asset, `stageArchive`, sets `staged = version`, `err = nil`, and
    restarts with the same delayed goroutine as `Install` (extracted into a shared helper).
  - Failures set `u.err` and log like `Stage`.
- `Check`, `Stage`, `Install` and the background `checkUpdates` loop are unchanged: they look
  only at the latest stable release. On a beta build the Stable channel reports an update only
  when a stable release with a higher version exists.

### 3.3 Config (`internal/config/config.go`)

- `System.UpdateChannel string`, persisted as `system.updates.channel`.
- Load: empty → `stable`. Defaults: `stable`.
- The loader rejects unknown keys (`KnownFields(true)`), so an older build would refuse a config
  carrying `channel`. The key is written **only when it is `beta`** (`omitempty`, stable encoded
  as empty): a Stable config stays readable by every build.
- `Validate`: value must be `stable` or `beta`.
- `run.go` passes it into `UpdatePolicy.Channel`.

### 3.4 Web UI API (`internal/webui/server.go`)

- `UpdateProvider` gains `Prereleases(ctx)` and `InstallVersion(ctx, version)`.
- `PUT /webui/api/management/update/channel` body `{"channel":"stable"|"beta"}`: same-origin
  check, writes config, answers the fresh `UpdateStatus`. Invalid value → 400.
- `GET /webui/api/management/update/prereleases` → `{"prereleases":[...]}`; 503 when updates are
  unavailable or the fetch fails.
- `POST /webui/api/management/update` accepts an optional body `{"version":"..."}`. Empty body or
  no version → today's `Install`. With a version → `InstallVersion`; `ErrReleaseNotFound` → 404,
  `ErrReleaseAlreadyInstalled` → 409, other errors → 503 as today.
- The `/api/v3` (Home Assistant) handlers are not changed. `UpdateStatus.channel` appears there as
  an extra field, which is harmless.

### 3.5 Web UI page (`index.html`, `app.js`)

- A **Channel** selector (Stable / Beta) at the top of the Updates page; changing it calls the
  channel endpoint and re-renders.
- Stable: the page as today.
- Beta: the Actions card shows the list of pre-releases (version, publish date, an "Installed"
  badge on the current one) with an **Install** button per row, disabled on the installed one.
  Install asks for confirmation (downgrade wording when the target is older), then posts
  `{"version"}` and shows the same "restarting" toast as today. Empty list → "No pre-releases
  published". The Latest version row stays: it shows the latest stable.

### 3.6 Release workflow

`softprops/action-gh-release` gets `prerelease: ${{ contains(github.ref_name, '-') }}`.

## 4. Error handling

| Situation | Result |
|---|---|
| Build without release repo, or updates disabled | `ErrUpdateUnavailable` → 503, the page shows "unavailable" as today |
| GitHub list fails / not an array | 503 on the prereleases endpoint, message in the page |
| One release in the list is malformed | Skipped, the rest is listed |
| Install of a tag that is not a pre-release | 404 |
| Install of the installed tag | 409 |
| Digest mismatch / bad archive | Same as `Stage` today: error stored, nothing staged |

## 5. Testing

- `release_test.go`: list parsing (drafts, invalid entries skipped, non-array rejected), `List`
  via the fake HTTP client, client URL derivation.
- `update_test.go`: `Prereleases` filters stable and sorts newest first, marks installed;
  `InstallVersion` stages an older beta (downgrade), refuses a stable tag, refuses the installed
  tag, triggers restart; `Status` reports the channel.
- `config` tests: channel round trip, empty → stable, invalid rejected.
- `webui/server_test.go`: channel PUT, prereleases GET, install with and without version, error
  mapping.
- Run the `system` tests on Linux if `stageArchive`'s `/bin/tar` is unavailable on Windows.

## 6. Out of scope

- Beta selection through the Home Assistant API.
- Automatic background updates on the Beta channel.
- Pagination beyond the 50 most recent releases.

# Update Channel (Stable / Beta) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the Web UI choose between the Stable update channel (today's behaviour) and a Beta channel that lists the companion's GitHub pre-releases and installs the one the user picks.

**Architecture:** `GitHubReleaseClient` learns to list releases (`/releases?per_page=50`); `Updater` gains `Prereleases` and `InstallVersion`, reusing the existing staging pipeline; the channel is persisted as `system.updates.channel`; the Web UI gets two endpoints and a channel selector on the Updates page. The Home Assistant API and the 3 h background check stay Stable.

**Tech Stack:** Go 1.26 (stdlib `net/http`, `encoding/json`, `gopkg.in/yaml`), vanilla JS/HTML Web UI, GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-10-28-update-channel-design.md`

## Global Constraints

- Channels are exactly `stable` and `beta`; empty means `stable`.
- Beta lists **every** published pre-release (draft excluded), newest first by semver, downgrade allowed; the running version is marked `installed`.
- `InstallVersion` accepts **only** a tag that is a pre-release in the listing; a stable tag → `ErrReleaseNotFound`; the running tag → `ErrReleaseAlreadyInstalled`.
- `Check`, `Stage`, `Install`, the `checkUpdates` loop in `internal/app/run.go` and every `/api/v3` handler keep their current behaviour.
- The config loader uses `KnownFields(true)`: an older build fails on unknown keys. The `channel` key is therefore written **only when it is `beta`** (`omitempty`, stable encoded as empty), so a Stable config stays readable by any build.
- Release list body limit: 4 MiB. Single release body limit stays 1 MiB.
- Code style: match the surrounding code — `fmt.Errorf("...: %w", err)`, `//nolint:errcheck // reason` on deferred closes, `t.Parallel()` in every test, table-free tests in the style of the existing files.
- **Testing on this Windows host:** `internal/webui` and `internal/app` do not build on Windows (they import `internal/media`, POSIX-only). Verify them with `GOOS=linux go vet ./internal/webui/ ./internal/app/` (this type-checks the test files too) and do **not** try to run them through WSL. `internal/system` tests that build an archive call `/bin/tar` and fail on Windows; `internal/config` tests that create a config file fail on Windows with "sync private file directory: Access is denied". These are pre-existing failures. Run only the new tests by name with `-run`, and state in the report which tests were written but not run.
- Commit messages end with: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`

## File map

| File | Change |
|---|---|
| `internal/system/release.go` | `ReleaseManifest` fields, `validateReleaseManifest`, `ParseGitHubReleaseList`, client takes the repo base URL, `List` |
| `internal/system/release_test.go` | List parsing and client tests |
| `internal/system/update.go` | Channel constants, `UpdatePolicy.Channel`, `UpdateStatus.Channel`, `Prerelease`, `ReleaseSource.List`, `Prereleases`, `InstallVersion`, shared staging/restart helpers |
| `internal/system/update_test.go` | Updater tests, `testReleaseSource.List` |
| `internal/config/config.go` | `System.UpdateChannel`, `persistedUpdates`, load default, validation |
| `internal/config/config_test.go` | Channel tests |
| `internal/app/run.go` | Client base URL, `Channel` in the policy |
| `internal/webui/server.go` | `UpdateProvider` methods, two routes, install with version |
| `internal/webui/server_test.go` | Handler tests, fake provider |
| `internal/webui/static/index.html`, `app.js` | Channel selector, pre-release list |
| `.github/workflows/release.yaml` | `prerelease:` |
| `docs/architecture.md` | Route table |

## Task order and lanes

- Lane A: Task 1 → Task 3
- Lane B: Task 2
- Lane C: Task 6
- Then Task 4 (needs 1, 2, 3), then Task 5 (needs 4).

---

### Task 1: Release listing in the GitHub client

**Files:**
- Modify: `internal/system/release.go`
- Modify: `internal/system/release_test.go`
- Modify: `internal/app/run.go:439` (constructor argument only)

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `ReleaseManifest{TagName string; Prerelease bool; Draft bool; PublishedAt time.Time; Assets []ReleaseAsset}` (JSON `tag_name`, `prerelease`, `draft`, `published_at`, `assets`)
  - `func ParseGitHubReleaseList(reader io.Reader) ([]ReleaseManifest, error)`
  - `func NewGitHubReleaseClient(client HTTPDoer, repoURL string) *GitHubReleaseClient` — `repoURL` is `https://api.github.com/repos/<owner>/<repo>`
  - `func (c *GitHubReleaseClient) List(ctx context.Context) ([]ReleaseManifest, error)`

- [ ] **Step 1: Write the failing tests**

In `internal/system/release_test.go`, change the constructor call inside `TestGitHubReleaseClient` and extend its final assertion:

```go
	releases := NewGitHubReleaseClient(client, "https://api.github.com/repos/example/companion")
```

```go
	if client.requests[0].URL.String() != "https://api.github.com/repos/example/companion/releases/latest" || client.requests[0].Header.Get("Accept") != "application/vnd.github+json" || client.requests[1].URL.String() != asset.DownloadURL {
		t.Fatalf("requests = %#v", client.requests)
	}
```

Append these tests (add `"time"` to the imports):

```go
const testReleaseList = `[
	{"tag_name":"v1.3.0","prerelease":false,"published_at":"2026-10-20T10:00:00Z","assets":[{"name":"companion.tar.gz","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","browser_download_url":"https://downloads.example.test/v1.3.0.tar.gz"}]},
	{"tag_name":"v1.4.0-beta.1","prerelease":true,"published_at":"2026-10-25T10:00:00Z","assets":[{"name":"companion.tar.gz","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","browser_download_url":"https://downloads.example.test/v1.4.0-beta.1.tar.gz"}]},
	{"tag_name":"v1.4.0-beta.2","prerelease":true,"draft":true,"published_at":null,"assets":[]},
	{"tag_name":"v1.4.0-beta.0","prerelease":true,"published_at":"2026-10-21T10:00:00Z","assets":[{"name":"companion.tar.gz","digest":"md5:abc","browser_download_url":"https://downloads.example.test/broken.tar.gz"}]}
]`

func TestParseGitHubReleaseListSkipsDraftsAndInvalidEntries(t *testing.T) {
	t.Parallel()

	releases, err := ParseGitHubReleaseList(strings.NewReader(testReleaseList))
	if err != nil {
		t.Fatal(err)
	}

	if len(releases) != 2 || releases[0].TagName != "v1.3.0" || releases[0].Prerelease || releases[1].TagName != "v1.4.0-beta.1" || !releases[1].Prerelease {
		t.Fatalf("releases = %#v", releases)
	}

	if want := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC); !releases[1].PublishedAt.Equal(want) {
		t.Fatalf("published_at = %v", releases[1].PublishedAt)
	}
}

func TestParseGitHubReleaseListRejectsNonArray(t *testing.T) {
	t.Parallel()

	if _, err := ParseGitHubReleaseList(strings.NewReader(`{"tag_name":"v1.3.0","assets":[]}`)); !errors.Is(err, ErrInvalidReleaseManifest) {
		t.Fatalf("ParseGitHubReleaseList() error = %v", err)
	}
}

func TestGitHubReleaseClientList(t *testing.T) {
	t.Parallel()

	client := &testHTTPClient{responses: []*http.Response{response(http.StatusOK, testReleaseList)}} //nolint:bodyclose // consumed by production code
	releases := NewGitHubReleaseClient(client, "https://api.github.com/repos/example/companion")

	list, err := releases.List(context.Background())
	if err != nil || len(list) != 2 {
		t.Fatalf("List() = %#v, err = %v", list, err)
	}

	if client.requests[0].URL.String() != "https://api.github.com/repos/example/companion/releases?per_page=50" || client.requests[0].Header.Get("Accept") != "application/vnd.github+json" {
		t.Fatalf("request = %#v", client.requests[0])
	}
}

func TestGitHubReleaseClientListRejectsErrorStatus(t *testing.T) {
	t.Parallel()

	client := &testHTTPClient{responses: []*http.Response{response(http.StatusInternalServerError, "boom")}} //nolint:bodyclose // closed by production code
	if _, err := NewGitHubReleaseClient(client, "https://api.github.com/repos/example/companion").List(context.Background()); err == nil || !strings.Contains(err.Error(), "release list response") {
		t.Fatalf("List() error = %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/system/ -run 'ReleaseList|GitHubReleaseClient' -v`
Expected: build failure — `undefined: ParseGitHubReleaseList`, `releases.List undefined`.

- [ ] **Step 3: Implement**

In `internal/system/release.go`, add `"time"` to the imports and replace `ReleaseManifest` and `ParseGitHubReleaseManifest` with:

```go
type ReleaseManifest struct {
	TagName     string         `json:"tag_name"`
	Prerelease  bool           `json:"prerelease"`
	Draft       bool           `json:"draft"`
	PublishedAt time.Time      `json:"published_at"`
	Assets      []ReleaseAsset `json:"assets"`
}

func ParseGitHubReleaseManifest(reader io.Reader) (ReleaseManifest, error) {
	var manifest ReleaseManifest

	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&manifest); err != nil {
		return ReleaseManifest{}, fmt.Errorf("decode release manifest: %w", ErrInvalidReleaseManifest)
	}

	if decoder.Decode(&struct{}{}) != io.EOF {
		return ReleaseManifest{}, ErrInvalidReleaseManifest
	}

	if err := validateReleaseManifest(manifest); err != nil {
		return ReleaseManifest{}, err
	}

	return manifest, nil
}

// ParseGitHubReleaseList decodes a GitHub release listing. Drafts and entries
// that fail manifest validation are dropped, so one broken release does not
// hide the others.
func ParseGitHubReleaseList(reader io.Reader) ([]ReleaseManifest, error) {
	var manifests []ReleaseManifest

	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&manifests); err != nil {
		return nil, fmt.Errorf("decode release list: %w", ErrInvalidReleaseManifest)
	}

	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, ErrInvalidReleaseManifest
	}

	releases := make([]ReleaseManifest, 0, len(manifests))
	for _, manifest := range manifests {
		if manifest.Draft || validateReleaseManifest(manifest) != nil {
			continue
		}

		releases = append(releases, manifest)
	}

	return releases, nil
}

func validateReleaseManifest(manifest ReleaseManifest) error {
	if manifest.TagName == "" {
		return ErrInvalidReleaseManifest
	}

	for _, asset := range manifest.Assets {
		if asset.Name == "" || asset.Digest == "" || asset.DownloadURL == "" {
			return ErrInvalidReleaseManifest
		}

		if _, err := parseSHA256Digest(asset.Digest); err != nil {
			return fmt.Errorf("asset %q: %w", asset.Name, err)
		}

		parsedURL, err := url.Parse(asset.DownloadURL)
		if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" {
			return fmt.Errorf("asset %q: %w", asset.Name, ErrInvalidReleaseManifest)
		}
	}

	return nil
}
```

Replace `GitHubReleaseClient`, `NewGitHubReleaseClient` and `Latest` with (leave `Download` as it is):

```go
type GitHubReleaseClient struct {
	client  HTTPDoer
	repoURL string
}

// NewGitHubReleaseClient takes the repository API base,
// https://api.github.com/repos/<owner>/<repo>.
func NewGitHubReleaseClient(client HTTPDoer, repoURL string) *GitHubReleaseClient {
	return &GitHubReleaseClient{client: client, repoURL: strings.TrimSuffix(repoURL, "/")}
}

func (c *GitHubReleaseClient) Latest(ctx context.Context) (ReleaseManifest, error) {
	if c == nil || c.client == nil || c.repoURL == "" {
		return ReleaseManifest{}, ErrRuntimeUnavailable
	}

	body, err := c.get(ctx, c.repoURL+"/releases/latest", "latest release")
	if err != nil {
		return ReleaseManifest{}, err
	}
	defer body.Close() //nolint:errcheck // response already validated

	return ParseGitHubReleaseManifest(io.LimitReader(body, 1<<20))
}

// List returns the 50 most recent published releases, pre-releases included.
func (c *GitHubReleaseClient) List(ctx context.Context) ([]ReleaseManifest, error) {
	if c == nil || c.client == nil || c.repoURL == "" {
		return nil, ErrRuntimeUnavailable
	}

	body, err := c.get(ctx, c.repoURL+"/releases?per_page=50", "release list")
	if err != nil {
		return nil, err
	}
	defer body.Close() //nolint:errcheck // response already validated

	return ParseGitHubReleaseList(io.LimitReader(body, 4<<20))
}

func (c *GitHubReleaseClient) get(ctx context.Context, target, what string) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("create %s request: %w", what, err)
	}

	request.Header.Set("Accept", "application/vnd.github+json")

	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", what, err)
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		response.Body.Close() //nolint:errcheck,gosec // status error is returned instead

		return nil, fmt.Errorf("%s response: %s", what, response.Status)
	}

	return response.Body, nil
}
```

In `internal/app/run.go` change the constructor call to:

```go
		source = system.NewGitHubReleaseClient(&http.Client{Timeout: 30 * time.Second}, "https://api.github.com/repos/"+build.ReleaseRepo)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/system/ -run 'ReleaseList|GitHubReleaseClient|ParseGitHubReleaseManifest|VerifySHA256' -v`
Expected: all PASS.
Run: `GOOS=linux go vet ./internal/system/ ./internal/app/`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/system/release.go internal/system/release_test.go internal/app/run.go
git commit -m "feat(system): list GitHub releases, pre-releases included

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Persist the update channel in config

**Files:**
- Modify: `internal/config/config.go` (`System` ~line 98, `persistedSystem` ~line 110, `Default` ~line 228, `Load` ~line 285, `Validate` ~line 419, `encode` ~line 509)
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.UpdateChannelStable = "stable"`, `config.UpdateChannelBeta = "beta"`, field `config.System.UpdateChannel string`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go` (`legacyConfigWithoutSIP` is the existing fixture, with no `channel` key):

```go
func TestUpdateChannelDefaultsToStableAndPersistsOnlyBeta(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(source, []byte(legacyConfigWithoutSIP), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	loaded, err := Load(source)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if loaded.System.UpdateChannel != UpdateChannelStable {
		t.Fatalf("UpdateChannel = %q", loaded.System.UpdateChannel)
	}

	for channel, want := range map[string]string{UpdateChannelStable: "", UpdateChannelBeta: UpdateChannelBeta} {
		loaded.System.UpdateChannel = channel

		data, err := encode(loaded)
		if err != nil {
			t.Fatalf("encode() error = %v", err)
		}

		var document struct {
			System struct {
				Updates map[string]any `yaml:"updates"`
			} `yaml:"system"`
		}
		if err := yaml.Unmarshal(data, &document); err != nil {
			t.Fatalf("decode encoded config: %v", err)
		}

		got, present := document.System.Updates["channel"]
		if want == "" && present || want != "" && got != want {
			t.Fatalf("channel %q encoded as %v (present %t)", channel, got, present)
		}
	}
}

func TestValidateUpdateChannel(t *testing.T) {
	t.Parallel()

	cfg, err := Default(Metadata{Model: "C300X", MAC: "00:11:22:33:44:55"})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.System.UpdateChannel != UpdateChannelStable {
		t.Fatalf("default UpdateChannel = %q", cfg.System.UpdateChannel)
	}

	for channel, valid := range map[string]bool{"": true, UpdateChannelStable: true, UpdateChannelBeta: true, "nightly": false} {
		cfg.System.UpdateChannel = channel
		if err := Validate(cfg); (err == nil) != valid {
			t.Fatalf("Validate(channel %q) error = %v", channel, err)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'UpdateChannel' -v`
Expected: build failure — `undefined: UpdateChannelStable`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`:

Add near the other package constants:

```go
const (
	UpdateChannelStable = "stable"
	UpdateChannelBeta   = "beta"
)
```

`System` gets the field:

```go
type System struct {
	RebootEnabled bool               `yaml:"reboot_enabled"`
	UpdateEnabled bool               `yaml:"update_enabled"`
	UpdateExposed bool               `yaml:"update_exposed"`
	UpdateChannel string             `yaml:"update_channel"`
	Services      map[string]Service `yaml:"services"`
}
```

Replace the anonymous `Updates` struct in `persistedSystem` with a named type:

```go
type persistedSystem struct {
	Reboot struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"reboot"`
	Updates  persistedUpdates   `yaml:"updates"`
	Services map[string]Service `yaml:"services"`
}

// persistedUpdates omits a stable channel: the loader rejects unknown keys, so
// writing it would make the config unreadable for builds older than channels.
type persistedUpdates struct {
	Enabled bool   `yaml:"enabled"`
	Exposed bool   `yaml:"exposed"`
	Channel string `yaml:"channel,omitempty"`
}
```

In `Default`, add `UpdateChannel: UpdateChannelStable,` after `UpdateExposed: false,`.

In `Load`, set the field in the `System` literal and default it right after the `cfg := Config{...}` literal:

```go
		System:  System{RebootEnabled: persisted.System.Reboot.Enabled, UpdateEnabled: persisted.System.Updates.Enabled, UpdateExposed: persisted.System.Updates.Exposed, UpdateChannel: persisted.System.Updates.Channel, Services: persisted.System.Services},
```

```go
	if cfg.System.UpdateChannel == "" {
		cfg.System.UpdateChannel = UpdateChannelStable
	}
```

In `Validate`, before the final `return nil`:

```go
	if channel := cfg.System.UpdateChannel; channel != "" && channel != UpdateChannelStable && channel != UpdateChannelBeta {
		return fmt.Errorf("invalid update channel %q", channel)
	}
```

In `encode`, replace the `Updates: struct{...}{...}` literal with:

```go
		}{Enabled: cfg.System.RebootEnabled}, Updates: persistedUpdates{Enabled: cfg.System.UpdateEnabled, Exposed: cfg.System.UpdateExposed, Channel: persistedChannel(cfg.System.UpdateChannel)}, Services: cfg.System.Services},
```

and add after `encode`:

```go
func persistedChannel(channel string) string {
	if channel == UpdateChannelStable {
		return ""
	}

	return channel
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -run 'UpdateChannel|TestEncodeAddsTheSIPSection' -v`
Expected: PASS. (The full package has 6 pre-existing Windows-only failures, see Global Constraints.)

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): persist the update channel, stable by default

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Updater pre-release listing and install by version

**Files:**
- Modify: `internal/system/update.go`
- Modify: `internal/system/update_test.go`

**Interfaces:**
- Consumes (Task 1): `ReleaseManifest.Prerelease`, `ReleaseManifest.PublishedAt`, `(*GitHubReleaseClient).List`.
- Produces:
  - `const UpdateChannelStable = "stable"`, `UpdateChannelBeta = "beta"`
  - `var ErrReleaseNotFound`, `ErrReleaseAlreadyInstalled`
  - `UpdatePolicy.Channel string`, `UpdateStatus.Channel string` (JSON `channel`)
  - `type Prerelease struct{ Version string; PublishedAt time.Time; Installed bool }` (JSON `version`, `published_at`, `installed`)
  - `ReleaseSource` gains `List(context.Context) ([]ReleaseManifest, error)`
  - `func (u *Updater) Prereleases(ctx context.Context) ([]Prerelease, error)`
  - `func (u *Updater) InstallVersion(ctx context.Context, version string) (UpdateStatus, error)`

- [ ] **Step 1: Write the failing tests**

In `internal/system/update_test.go`, replace `testReleaseSource` and its methods with:

```go
type testReleaseSource struct {
	manifest ReleaseManifest
	releases []ReleaseManifest
	listErr  error
	data     []byte
}

func (s *testReleaseSource) Latest(context.Context) (ReleaseManifest, error) { return s.manifest, nil }
func (s *testReleaseSource) List(context.Context) ([]ReleaseManifest, error) {
	return s.releases, s.listErr
}

func (s *testReleaseSource) Download(context.Context, ReleaseAsset) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}
```

Append these tests:

```go
func testPrereleaseUpdater(t *testing.T, current string, source *testReleaseSource, restart RestartFunc) (*Updater, string) {
	t.Helper()
	dir := t.TempDir()

	return NewUpdater(source, BuildInfo{Version: current, ReleaseRepo: "owner/repo"}, func() UpdatePolicy {
		return UpdatePolicy{Enabled: true, Channel: UpdateChannelBeta, DataDir: dir}
	}, restart), dir
}

func TestUpdaterPrereleasesListsOnlyPrereleasesNewestFirst(t *testing.T) {
	t.Parallel()

	published := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	updater, _ := testPrereleaseUpdater(t, "v1.3.0-beta.2", &testReleaseSource{releases: []ReleaseManifest{
		{TagName: "v1.3.0"},
		{TagName: "v1.3.0-beta.2", Prerelease: true},
		{TagName: "v1.2.0-rc.1", Prerelease: true},
		{TagName: "v1.3.0-beta.10", Prerelease: true, PublishedAt: published},
		{TagName: "nightly", Prerelease: true},
	}}, nil)

	prereleases, err := updater.Prereleases(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	want := []Prerelease{{Version: "v1.3.0-beta.10", PublishedAt: published}, {Version: "v1.3.0-beta.2", Installed: true}, {Version: "v1.2.0-rc.1"}}
	if len(prereleases) != len(want) {
		t.Fatalf("prereleases = %#v", prereleases)
	}

	for i := range want {
		if prereleases[i].Version != want[i].Version || prereleases[i].Installed != want[i].Installed || !prereleases[i].PublishedAt.Equal(want[i].PublishedAt) {
			t.Fatalf("prereleases[%d] = %#v, want %#v", i, prereleases[i], want[i])
		}
	}
}

func TestUpdaterPrereleasesReturnsListError(t *testing.T) {
	t.Parallel()

	listErr := errors.New("github down")
	updater, _ := testPrereleaseUpdater(t, "v1.3.0", &testReleaseSource{listErr: listErr}, nil)

	if _, err := updater.Prereleases(context.Background()); !errors.Is(err, listErr) {
		t.Fatalf("Prereleases() error = %v", err)
	}
}

func TestUpdaterPrereleasesUnavailableWhenUpdatesDisabled(t *testing.T) {
	t.Parallel()

	updater := NewUpdater(&testReleaseSource{}, BuildInfo{Version: "v1.3.0", ReleaseRepo: "owner/repo"}, func() UpdatePolicy { return UpdatePolicy{} }, nil)
	if _, err := updater.Prereleases(context.Background()); !errors.Is(err, ErrUpdateUnavailable) {
		t.Fatalf("Prereleases() error = %v", err)
	}
}

func TestUpdaterInstallVersionRefusesStableTag(t *testing.T) {
	t.Parallel()

	updater, dir := testPrereleaseUpdater(t, "v1.3.0-beta.2", &testReleaseSource{releases: []ReleaseManifest{{TagName: "v1.2.0"}}}, func(context.Context) error { return nil })

	if _, err := updater.InstallVersion(context.Background(), "v1.2.0"); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("InstallVersion() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "companion.new")); !os.IsNotExist(err) {
		t.Fatalf("staged binary exists, err = %v", err)
	}
}

func TestUpdaterInstallVersionRefusesInstalledVersion(t *testing.T) {
	t.Parallel()

	updater, _ := testPrereleaseUpdater(t, "v1.3.0-beta.2", &testReleaseSource{releases: []ReleaseManifest{{TagName: "v1.3.0-beta.2", Prerelease: true}}}, func(context.Context) error { return nil })

	if _, err := updater.InstallVersion(context.Background(), "v1.3.0-beta.2"); !errors.Is(err, ErrReleaseAlreadyInstalled) {
		t.Fatalf("InstallVersion() error = %v", err)
	}
}

func TestUpdaterInstallVersionRequiresRestart(t *testing.T) {
	t.Parallel()

	updater, _ := testPrereleaseUpdater(t, "v1.3.0", &testReleaseSource{}, nil)
	if _, err := updater.InstallVersion(context.Background(), "v1.3.0-beta.1"); !errors.Is(err, ErrUpdateUnavailable) {
		t.Fatalf("InstallVersion() error = %v", err)
	}
}

// Needs /bin/tar: does not run on Windows.
func TestUpdaterInstallVersionStagesOlderPrereleaseThenRestarts(t *testing.T) {
	t.Parallel()

	binary := []byte("older beta")
	data := testReleaseArchive(t, companionBundlePath, binary)
	digest := sha256.Sum256(data)
	restarted := make(chan struct{}, 1)
	updater, dir := testPrereleaseUpdater(t, "v1.3.0-beta.2", &testReleaseSource{
		releases: []ReleaseManifest{{TagName: "v1.3.0-beta.1", Prerelease: true, Assets: []ReleaseAsset{{Name: companionAssetName, Digest: "sha256:" + hex.EncodeToString(digest[:])}}}},
		data:     data,
	}, func(context.Context) error {
		restarted <- struct{}{}
		return nil
	})

	status, err := updater.InstallVersion(context.Background(), "v1.3.0-beta.1")
	if err != nil {
		t.Fatal(err)
	}

	if status.StagedVersion != "v1.3.0-beta.1" || status.Stage != "staged" || status.LatestVersion != "v1.3.0-beta.2" {
		t.Fatalf("status = %#v", status)
	}

	staged, err := os.ReadFile(filepath.Join(dir, "companion.new"))
	if err != nil || !bytes.Equal(staged, binary) {
		t.Fatalf("staged = %q, err = %v", staged, err)
	}

	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart was not requested")
	}
}

func TestUpdaterStatusReportsChannel(t *testing.T) {
	t.Parallel()

	for policyChannel, want := range map[string]string{"": UpdateChannelStable, UpdateChannelStable: UpdateChannelStable, UpdateChannelBeta: UpdateChannelBeta} {
		updater := NewUpdater(&testReleaseSource{}, BuildInfo{Version: "v1.2.3", ReleaseRepo: "owner/repo"}, func() UpdatePolicy {
			return UpdatePolicy{Enabled: true, Channel: policyChannel}
		}, nil)

		status, err := updater.Status(context.Background())
		if err != nil || status.Channel != want {
			t.Fatalf("policy channel %q: status = %#v, err = %v", policyChannel, status, err)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/system/ -run 'Prerelease|InstallVersion|ReportsChannel' -v`
Expected: build failure — `undefined: UpdateChannelBeta`, `updater.Prereleases undefined`.

- [ ] **Step 3: Implement**

In `internal/system/update.go`, add `"slices"` and `"sort"` to the imports.

Replace the `var ErrUpdateUnavailable = ...` line with:

```go
var (
	ErrUpdateUnavailable       = errors.New("system: update control is unavailable")
	ErrReleaseNotFound         = errors.New("system: release is not a published pre-release")
	ErrReleaseAlreadyInstalled = errors.New("system: release is already installed")
)

const (
	UpdateChannelStable = "stable"
	UpdateChannelBeta   = "beta"
)
```

Replace `UpdatePolicy`, `UpdateStatus` and `ReleaseSource` with:

```go
type UpdatePolicy struct {
	Enabled bool
	Exposed bool
	Channel string
	DataDir string
}

func (p UpdatePolicy) channel() string {
	if p.Channel == UpdateChannelBeta {
		return UpdateChannelBeta
	}

	return UpdateChannelStable
}

type UpdateStatus struct {
	Enabled         bool   `json:"enabled"`
	Exposed         bool   `json:"exposed"`
	Channel         string `json:"channel"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	StagedVersion   string `json:"staged_version,omitempty"`
	RestartRequired bool   `json:"restart_required"`
	Stage           string `json:"stage"`
	Error           string `json:"error,omitempty"`
}

// Prerelease is one entry of the Beta channel listing.
type Prerelease struct {
	Version     string    `json:"version"`
	PublishedAt time.Time `json:"published_at"`
	Installed   bool      `json:"installed"`
}

type ReleaseSource interface {
	Latest(context.Context) (ReleaseManifest, error)
	List(context.Context) ([]ReleaseManifest, error)
	Download(context.Context, ReleaseAsset) (io.ReadCloser, error)
}
```

In `Status`, set the channel in the first literal:

```go
	status := UpdateStatus{Enabled: policy.Enabled, Exposed: policy.Exposed, Channel: policy.channel(), CurrentVersion: u.build.Version}
```

Replace the body of `Stage` from `asset, err := manifest.Asset(companionAssetName)` to the end of the function with:

```go
	if err := u.stageManifest(ctx, manifest); err != nil {
		return UpdateStatus{}, err
	}

	u.mu.Lock()
	u.latest = manifest.TagName
	u.mu.Unlock()

	return u.Status(ctx)
}
```

Replace the whole `Install` function with:

```go
// Install stages the newest release and restarts after the caller receives its response.
func (u *Updater) Install(ctx context.Context) (UpdateStatus, error) {
	if u == nil || u.restart == nil {
		return UpdateStatus{}, ErrUpdateUnavailable
	}

	status, err := u.Stage(ctx)
	if err != nil {
		return UpdateStatus{}, err
	}

	if !status.RestartRequired {
		return status, nil
	}

	u.restartLater(ctx, status.StagedVersion)

	return status, nil
}

// Prereleases lists the published pre-releases newest first, marking the running one.
func (u *Updater) Prereleases(ctx context.Context) ([]Prerelease, error) {
	if err := u.available(); err != nil {
		return nil, err
	}

	manifests, err := u.prereleaseManifests(ctx)
	if err != nil {
		u.logger.WarnContext(ctx, "pre-release list failed", "error", err)
		return nil, err
	}

	prereleases := make([]Prerelease, 0, len(manifests))
	for _, manifest := range manifests {
		prereleases = append(prereleases, Prerelease{Version: manifest.TagName, PublishedAt: manifest.PublishedAt, Installed: manifest.TagName == u.build.Version})
	}

	return prereleases, nil
}

// InstallVersion stages the named pre-release, older ones included, and
// restarts after the caller receives its response.
func (u *Updater) InstallVersion(ctx context.Context, version string) (UpdateStatus, error) {
	if u == nil || u.restart == nil {
		return UpdateStatus{}, ErrUpdateUnavailable
	}

	if err := u.available(); err != nil {
		return UpdateStatus{}, err
	}

	if version == u.build.Version {
		return UpdateStatus{}, ErrReleaseAlreadyInstalled
	}

	manifests, err := u.prereleaseManifests(ctx)
	if err != nil {
		return UpdateStatus{}, u.stageFailed(ctx, version, err)
	}

	index := slices.IndexFunc(manifests, func(manifest ReleaseManifest) bool { return manifest.TagName == version })
	if index < 0 {
		return UpdateStatus{}, ErrReleaseNotFound
	}

	u.logger.InfoContext(ctx, "pre-release staging started", "current_version", u.build.Version, "target_version", version)

	if err := u.stageManifest(ctx, manifests[index]); err != nil {
		return UpdateStatus{}, err
	}

	status, err := u.Status(ctx)
	if err != nil {
		return UpdateStatus{}, err
	}

	u.restartLater(ctx, status.StagedVersion)

	return status, nil
}

func (u *Updater) prereleaseManifests(ctx context.Context) ([]ReleaseManifest, error) {
	manifests, err := u.source.List(ctx)
	if err != nil {
		return nil, err
	}

	prereleases := make([]ReleaseManifest, 0, len(manifests))
	for _, manifest := range manifests {
		if manifest.Prerelease && semverPattern.MatchString(manifest.TagName) {
			prereleases = append(prereleases, manifest)
		}
	}

	sort.SliceStable(prereleases, func(i, j int) bool { return newerVersion(prereleases[i].TagName, prereleases[j].TagName) })

	return prereleases, nil
}

// stageManifest downloads, verifies and stages the companion binary of manifest.
func (u *Updater) stageManifest(ctx context.Context, manifest ReleaseManifest) error {
	asset, err := manifest.Asset(companionAssetName)
	if err != nil {
		return u.stageFailed(ctx, manifest.TagName, err)
	}

	body, err := u.source.Download(ctx, asset)
	if err != nil {
		return u.stageFailed(ctx, manifest.TagName, err)
	}
	defer body.Close() //nolint:errcheck // read error is returned below

	if err := stageArchive(ctx, body, asset.Digest, u.currentPolicy().DataDir); err != nil {
		return u.stageFailed(ctx, manifest.TagName, err)
	}

	u.mu.Lock()
	u.staged, u.err = manifest.TagName, nil
	u.mu.Unlock()

	u.logger.InfoContext(ctx, "update staged", "target_version", manifest.TagName)

	return nil
}

func (u *Updater) stageFailed(ctx context.Context, version string, err error) error {
	u.setError(err)
	u.logger.ErrorContext(ctx, "update staging failed", "target_version", version, "error", err)

	return err
}

func (u *Updater) restartLater(ctx context.Context, version string) {
	go func() {
		time.Sleep(restartDelay)

		restartCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restartTimeout)
		defer cancel()

		if err := u.restart(restartCtx); err != nil {
			u.setError(fmt.Errorf("restart companion: %w", err))
			u.logger.Error("update restart failed", "staged_version", version, "error", err)

			return
		}

		u.logger.Info("update restart dispatched", "staged_version", version)
	}()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/system/ -run 'Prerelease|InstallVersionRefuses|InstallVersionRequires|ReportsChannel|ReleaseList|GitHubReleaseClient|NewerVersion|DoesNotUpdateSameVersion|UnavailableWithoutReleaseRepo|DefaultsLatestVersion|SerializesLatestVersion' -v`
Expected: all PASS.
`TestUpdaterInstallVersionStagesOlderPrereleaseThenRestarts` and the existing staging tests need `/bin/tar`: on Windows they fail with `exec: "/bin/tar": executable file not found`, the same as before this change. Do not try to work around it; report them as written but not run.
Run: `GOOS=linux go vet ./internal/system/ ./internal/app/ ./internal/api/`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/system/update.go internal/system/update_test.go
git commit -m "feat(system): list pre-releases and install one by version

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Web UI API for channel, pre-releases and install by version

**Files:**
- Modify: `internal/app/run.go:432-435` (policy closure)
- Modify: `internal/webui/server.go` (`UpdateProvider` ~line 44, routes ~line 152, `handleUpdateInstall` ~line 194, new handlers after `handleUpdateCheck`)
- Modify: `internal/webui/server_test.go` (`fakeUpdateProvider` ~line 637, new tests)

**Interfaces:**
- Consumes: `system.Prerelease`, `(*system.Updater).Prereleases`, `(*system.Updater).InstallVersion`, `system.ErrReleaseNotFound`, `system.ErrReleaseAlreadyInstalled` (Task 3); `config.UpdateChannelStable`, `config.UpdateChannelBeta`, `config.System.UpdateChannel` (Task 2).
- Produces (HTTP, used by Task 5):
  - `PUT /webui/api/management/update/channel` body `{"channel":"stable"|"beta"}` → 200 `UpdateStatus` JSON; 400 on an invalid channel.
  - `GET /webui/api/management/update/prereleases` → 200 `{"prereleases":[{"version","published_at","installed"}]}`; 503 on failure.
  - `POST /webui/api/management/update` optional body `{"version":"..."}` → 202 `UpdateStatus`; 404 not a pre-release; 409 already installed; 503 other failures.

- [ ] **Step 1: Write the failing tests**

In `internal/webui/server_test.go`, replace `fakeUpdateProvider` and its methods with:

```go
type fakeUpdateProvider struct {
	status         system.UpdateStatus
	check          func(context.Context) (system.UpdateStatus, error)
	install        func(context.Context) (system.UpdateStatus, error)
	prereleases    []system.Prerelease
	prereleasesErr error
	installVersion func(context.Context, string) (system.UpdateStatus, error)
}

func (f fakeUpdateProvider) Status(context.Context) (system.UpdateStatus, error) {
	return f.status, nil
}

func (f fakeUpdateProvider) Check(ctx context.Context) (system.UpdateStatus, error) {
	if f.check == nil {
		return f.status, nil
	}

	return f.check(ctx)
}

func (f fakeUpdateProvider) Install(ctx context.Context) (system.UpdateStatus, error) {
	if f.install == nil {
		return f.status, nil
	}

	return f.install(ctx)
}

func (f fakeUpdateProvider) Prereleases(context.Context) ([]system.Prerelease, error) {
	return f.prereleases, f.prereleasesErr
}

func (f fakeUpdateProvider) InstallVersion(ctx context.Context, version string) (system.UpdateStatus, error) {
	if f.installVersion == nil {
		return f.status, nil
	}

	return f.installVersion(ctx, version)
}
```

Append these tests:

```go
func TestUpdateChannelPersistsTheChoice(t *testing.T) {
	t.Parallel()

	server, store := testServer(t, nil)
	server.SetUpdate(fakeUpdateProvider{status: system.UpdateStatus{Channel: system.UpdateChannelBeta, Stage: "idle"}})

	response := request(t, server, http.MethodPut, "/webui/api/management/update/channel", map[string]string{"channel": "beta"}, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated channel status = %d", response.Code)
	}

	cookie := configuredSession(t, server)

	response = request(t, server, http.MethodPut, "/webui/api/management/update/channel", map[string]string{"channel": "beta"}, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("channel status = %d: %s", response.Code, response.Body.String())
	}

	if got := store.Snapshot().System.UpdateChannel; got != config.UpdateChannelBeta {
		t.Fatalf("UpdateChannel = %q", got)
	}

	response = request(t, server, http.MethodPut, "/webui/api/management/update/channel", map[string]string{"channel": "nightly"}, cookie)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid channel status = %d", response.Code)
	}

	if got := store.Snapshot().System.UpdateChannel; got != config.UpdateChannelBeta {
		t.Fatalf("UpdateChannel after invalid request = %q", got)
	}
}

func TestUpdatePrereleasesListsFromUpdater(t *testing.T) {
	t.Parallel()

	server, _ := testServer(t, nil)
	server.SetUpdate(fakeUpdateProvider{prereleases: []system.Prerelease{{Version: "v1.3.0-beta.1", Installed: true}}})

	response := request(t, server, http.MethodGet, "/webui/api/management/update/prereleases", nil, configuredSession(t, server))
	if response.Code != http.StatusOK {
		t.Fatalf("prereleases status = %d: %s", response.Code, response.Body.String())
	}

	var body struct {
		Prereleases []system.Prerelease `json:"prereleases"`
	}
	decodeResponse(t, response, &body)

	if len(body.Prereleases) != 1 || body.Prereleases[0].Version != "v1.3.0-beta.1" || !body.Prereleases[0].Installed {
		t.Fatalf("body = %#v", body)
	}
}

func TestUpdatePrereleasesUnavailable(t *testing.T) {
	t.Parallel()

	server, _ := testServer(t, nil)
	server.SetUpdate(fakeUpdateProvider{prereleasesErr: system.ErrUpdateUnavailable})

	response := request(t, server, http.MethodGet, "/webui/api/management/update/prereleases", nil, configuredSession(t, server))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("prereleases status = %d", response.Code)
	}
}

func TestUpdateInstallWithVersionInvokesInstallVersion(t *testing.T) {
	t.Parallel()

	versions := make(chan string, 1)
	server, _ := testServer(t, nil)
	server.SetUpdate(fakeUpdateProvider{
		install: func(context.Context) (system.UpdateStatus, error) {
			t.Error("Install called for a versioned request")
			return system.UpdateStatus{}, nil
		},
		installVersion: func(_ context.Context, version string) (system.UpdateStatus, error) {
			versions <- version
			return system.UpdateStatus{RestartRequired: true, StagedVersion: version}, nil
		},
	})

	response := request(t, server, http.MethodPost, "/webui/api/management/update", map[string]string{"version": "v1.3.0-beta.1"}, configuredSession(t, server))
	if response.Code != http.StatusAccepted {
		t.Fatalf("install status = %d: %s", response.Code, response.Body.String())
	}

	if got := <-versions; got != "v1.3.0-beta.1" {
		t.Fatalf("version = %q", got)
	}
}

func TestUpdateInstallWithVersionMapsErrors(t *testing.T) {
	t.Parallel()

	for err, want := range map[error]int{
		system.ErrReleaseNotFound:         http.StatusNotFound,
		system.ErrReleaseAlreadyInstalled: http.StatusConflict,
		errors.New("download failed"):     http.StatusServiceUnavailable,
	} {
		server, _ := testServer(t, nil)
		server.SetUpdate(fakeUpdateProvider{installVersion: func(context.Context, string) (system.UpdateStatus, error) {
			return system.UpdateStatus{}, err
		}})

		response := request(t, server, http.MethodPost, "/webui/api/management/update", map[string]string{"version": "v1.3.0-beta.1"}, configuredSession(t, server))
		if response.Code != want {
			t.Fatalf("error %v: status = %d, want %d", err, response.Code, want)
		}
	}
}
```

Add `"errors"` to the test imports if it is not already there.

- [ ] **Step 2: Verify the tests compile**

Run: `GOOS=linux go vet ./internal/webui/`
Expected: no output. The fake only gained methods, so the package still type-checks; the new tests would fail at run time (404 on the new routes, `Install` called instead of `InstallVersion`). This package cannot run its tests on Windows (see Global Constraints), so go straight to Step 3.

- [ ] **Step 3: Implement**

In `internal/app/run.go`, the policy closure becomes:

```go
	policy := func() system.UpdatePolicy {
		cfg := configStore.Snapshot().System
		return system.UpdatePolicy{Enabled: cfg.UpdateEnabled, Exposed: cfg.UpdateExposed, Channel: cfg.UpdateChannel, DataDir: system.CompanionDataDir}
	}
```

In `internal/webui/server.go`, extend `UpdateProvider`:

```go
	UpdateProvider      interface {
		Status(context.Context) (system.UpdateStatus, error)
		Check(context.Context) (system.UpdateStatus, error)
		Install(context.Context) (system.UpdateStatus, error)
		Prereleases(context.Context) ([]system.Prerelease, error)
		InstallVersion(context.Context, string) (system.UpdateStatus, error)
	}
```

Register the two routes right after `POST /webui/api/management/update`:

```go
	mux.HandleFunc("PUT /webui/api/management/update/channel", s.requireReady(s.handleUpdateChannel))
	mux.HandleFunc("GET /webui/api/management/update/prereleases", s.requireReady(s.handleUpdatePrereleases))
```

Replace `handleUpdateInstall` with:

```go
func (s *Server) handleUpdateInstall(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}

	if s.update == nil {
		writeError(w, http.StatusServiceUnavailable, "update control is unavailable")
		return
	}

	var body struct {
		Version string `json:"version"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}

	var (
		status system.UpdateStatus
		err    error
	)

	if body.Version == "" {
		status, err = s.update.Install(r.Context())
	} else {
		status, err = s.update.InstallVersion(r.Context(), body.Version)
	}

	switch {
	case errors.Is(err, system.ErrReleaseNotFound):
		writeError(w, http.StatusNotFound, "release is not a published pre-release")
		return
	case errors.Is(err, system.ErrReleaseAlreadyInstalled):
		writeError(w, http.StatusConflict, "release is already installed")
		return
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "update install failed")
		return
	}

	writeJSON(w, http.StatusAccepted, status)
}
```

Add after `handleUpdateCheck`:

```go
func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}

	if s.update == nil {
		writeError(w, http.StatusServiceUnavailable, "update control is unavailable")
		return
	}

	var body struct {
		Channel string `json:"channel"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	if body.Channel != config.UpdateChannelStable && body.Channel != config.UpdateChannelBeta {
		writeError(w, http.StatusBadRequest, "update channel must be stable or beta")
		return
	}

	if err := s.config.Update(func(cfg *config.Config) error {
		cfg.System.UpdateChannel = body.Channel
		return nil
	}); err != nil {
		writeError(w, http.StatusBadRequest, "configuration is invalid")
		return
	}

	status, err := s.update.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "update status is unavailable")
		return
	}

	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleUpdatePrereleases(w http.ResponseWriter, r *http.Request) {
	if s.update == nil {
		writeError(w, http.StatusServiceUnavailable, "update control is unavailable")
		return
	}

	prereleases, err := s.update.Prereleases(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "pre-release list is unavailable")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"prereleases": prereleases})
}
```

- [ ] **Step 4: Verify**

Run: `GOOS=linux go vet ./internal/webui/ ./internal/app/ ./internal/api/`
Expected: no output (this type-checks the new tests too).
Run: `go test ./internal/system/ ./internal/config/ -run 'Prerelease|InstallVersionRefuses|InstallVersionRequires|ReportsChannel|UpdateChannel'`
Expected: PASS.
The `internal/webui` tests cannot run on this host; report them as written but not run.

- [ ] **Step 5: Commit**

```bash
git add internal/app/run.go internal/webui/server.go internal/webui/server_test.go
git commit -m "feat(webui): choose the update channel and install a pre-release

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Updates page — channel selector and pre-release list

**Files:**
- Modify: `internal/webui/static/index.html:253-273` (Updates page)
- Modify: `internal/webui/static/app.js:889-967` (update functions)

**Interfaces:**
- Consumes (Task 4): the three HTTP endpoints; `UpdateStatus.channel`, `UpdateStatus.enabled`.
- Existing helpers used: `apiGet`, `apiPost`, `apiPut`, `escapeHtml`, `handleApiError(err, fallback)`, `setStatus`, `toast.show({kind, message})`, `requestConfirmation(title, message, acceptLabel, action)` (sets text with `textContent`), `pollRestartReady`.

- [ ] **Step 1: Markup**

In `index.html`, replace the whole `<div id="managementUpdatesPage" ...>` block with:

```html
    <div id="managementUpdatesPage" class="page hidden">
      <h1 class="page-title">Updates</h1>
      <p class="page-desc">Check for signed Companion releases, install an available update, and review the active release state.</p>
      <div class="card section">
        <div class="card-header">Channel</div>
        <p class="card-desc">Stable installs the latest release. Beta lists the published pre-releases so you can pick the one to install, older ones included.</p>
        <select id="updatesChannel" class="form-input filter-select" title="Update channel" onchange="changeUpdateChannel(this.value)">
          <option value="stable">Stable</option>
          <option value="beta">Beta</option>
        </select>
      </div>
      <div class="grid grid-2 section">
        <div class="card">
          <div class="card-header">Release</div>
          <div class="info-row"><span class="info-label">Current version</span><span id="updatesCurrentVersion" class="info-value mono">-</span></div>
          <div class="info-row"><span class="info-label">Latest stable</span><span id="updatesLatestVersion" class="info-value mono">-</span></div>
          <div class="info-row"><span class="info-label">Status</span><span class="info-value"><span id="updatesStageBadge" class="badge badge-info">-</span></span></div>
        </div>
        <div class="card">
          <div class="card-header">Actions</div>
          <p id="updatesDescription" class="card-desc">Loading update status.</p>
          <div id="updatesStableActions">
            <div class="header-actions">
              <button id="updatesCheck" class="btn btn-secondary btn-sm" type="button" onclick="checkForUpdates()">Check for Updates</button>
              <button id="updatesInstall" class="btn btn-primary btn-sm hidden" type="button" onclick="installUpdate()">Install Update</button>
            </div>
          </div>
          <div id="updatesBetaActions" class="hidden">
            <div id="updatesPrereleases"></div>
            <div class="header-actions">
              <button id="updatesRefreshPrereleases" class="btn btn-secondary btn-sm" type="button" onclick="loadPrereleases()">Refresh</button>
            </div>
          </div>
          <div id="updatesStatus" class="form-status" aria-live="polite"></div>
        </div>
      </div>
    </div>
```

- [ ] **Step 2: Script**

In `app.js`, add `var _updateStatus = null;` next to the other top-level `var` declarations (after `var _sessionRefreshPending = false;`).

At the end of `renderUpdateStatus(update)` (after the `install` toggle line), append:

```js
  _updateStatus = update;
  var beta = update.channel === 'beta';
  var usable = update.enabled && update.stage !== 'unavailable';
  var channel = document.getElementById('updatesChannel');
  if (channel) {
    channel.value = beta ? 'beta' : 'stable';
    channel.disabled = !usable;
  }
  document.getElementById('updatesStableActions').classList.toggle('hidden', beta);
  document.getElementById('updatesBetaActions').classList.toggle('hidden', !beta);
  if (beta && (update.stage === 'idle' || update.stage === 'available')) {
    document.getElementById('updatesDescription').textContent = 'Pick a pre-release to install. Companion restarts after installing it.';
  }
  if (beta && usable) loadPrereleases();
```

Add these functions after `installUpdate`:

```js
function changeUpdateChannel(channel) {
  var select = document.getElementById('updatesChannel');
  if (select) select.disabled = true;
  apiPut('/webui/api/management/update/channel', { channel: channel }).then(function(update) {
    renderUpdateStatus(update);
    toast.show({ kind: 'success', message: channel === 'beta' ? 'Beta channel selected.' : 'Stable channel selected.' });
  }).catch(function(err) {
    handleApiError(err, 'Failed to change update channel');
    loadUpdateStatus();
  });
}

function loadPrereleases() {
  var container = document.getElementById('updatesPrereleases');
  if (!container) return;
  container.innerHTML = '<p class="card-desc">Loading pre-releases...</p>';
  apiGet('/webui/api/management/update/prereleases').then(function(data) {
    renderPrereleases(data.prereleases || []);
  }).catch(function(err) {
    container.innerHTML = '<p class="card-desc">' + escapeHtml(err.message || 'Failed to load pre-releases.') + '</p>';
    handleApiError(err);
  });
}

function renderPrereleases(prereleases) {
  var container = document.getElementById('updatesPrereleases');
  if (!container) return;
  if (!prereleases.length) {
    container.innerHTML = '<p class="card-desc">No pre-releases published.</p>';
    return;
  }
  var installedIndex = prereleases.findIndex(function(p) { return p.installed; });
  container.innerHTML = prereleases.map(function(p, index) {
    var date = p.published_at ? new Date(p.published_at).toLocaleDateString() : '';
    var action = p.installed
      ? '<span class="badge badge-success">Installed</span>'
      : '<button class="btn btn-primary btn-sm" type="button" data-index="' + index + '">Install</button>';
    return '<div class="info-row"><span class="info-label"><span class="mono-inline">' + escapeHtml(p.version) + '</span>' +
      (date ? ' &middot; ' + escapeHtml(date) : '') + '</span><span class="info-value">' + action + '</span></div>';
  }).join('');
  container.querySelectorAll('button[data-index]').forEach(function(button) {
    var index = Number(button.getAttribute('data-index'));
    button.addEventListener('click', function() {
      installPrerelease(prereleases[index].version, installedIndex >= 0 && index > installedIndex, button);
    });
  });
}

function installPrerelease(version, downgrade, button) {
  var message = downgrade
    ? 'This pre-release is older than the running one: Companion will be downgraded to ' + version + ' and restart.'
    : 'Companion will install ' + version + ' and restart.';
  requestConfirmation('Install ' + version + '?', message, 'Install', function() {
    if (button) button.disabled = true;
    apiPost('/webui/api/management/update', { version: version }).then(function() {
      toast.show({ kind: 'success', message: 'Update installed. Companion is restarting...' });
      setTimeout(pollRestartReady, 1500);
    }).catch(function(err) {
      if (button) button.disabled = false;
      setStatus('updatesStatus', err.message || 'Update failed.', 'var(--danger)');
      handleApiError(err, 'Update failed');
    });
  });
}
```

Notes for the implementer: the downgrade test uses the list order (newest first), so it is only known when the running version is itself in the list; when a stable build is running the generic message is shown. `_updateStatus` is kept for debugging from the console and future use; do not add other uses.

- [ ] **Step 3: Verify**

Run: `node --check internal/webui/static/app.js`
Expected: no output. (If `node` is not installed, say so in the report.)
Run: `GOOS=linux go vet ./internal/webui/`
Expected: no output (the static files are embedded; this confirms the package still builds).
Read the final Updates block of `index.html` once: every `id` used in `app.js` (`updatesChannel`, `updatesStableActions`, `updatesBetaActions`, `updatesPrereleases`, `updatesRefreshPrereleases`, `updatesDescription`, `updatesStatus`, `updatesCheck`, `updatesInstall`) exists exactly once.

- [ ] **Step 4: Commit**

```bash
git add internal/webui/static/index.html internal/webui/static/app.js
git commit -m "feat(webui): pick the update channel and a beta to install

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Publish suffixed tags as pre-releases, document the routes

**Files:**
- Modify: `.github/workflows/release.yaml` (step "Publish GitHub release assets")
- Modify: `docs/architecture.md:255` (`/webui/api` route list)

- [ ] **Step 1: Workflow**

In the `softprops/action-gh-release@v3` step, add `prerelease` to `with:` right after `files:`'s block, keeping two-space YAML indentation:

```yaml
          prerelease: ${{ contains(github.ref_name, '-') }}
```

- [ ] **Step 2: Docs**

In `docs/architecture.md`, change `` `management/update{,/check}`, `` to:

```
`management/update{,/check,/channel,/prereleases}`,
```

- [ ] **Step 3: Verify**

Run: `python -c "import yaml,sys; yaml.safe_load(open('.github/workflows/release.yaml'))" && echo ok`
Expected: `ok`. (If Python or PyYAML is missing, check the indentation by reading the file and say so.)

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yaml docs/architecture.md
git commit -m "ci(release): publish suffixed tags as pre-releases

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

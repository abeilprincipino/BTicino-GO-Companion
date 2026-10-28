package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrUpdateUnavailable       = errors.New("system: update control is unavailable")
	ErrReleaseNotFound         = errors.New("system: release is not a published pre-release")
	ErrReleaseAlreadyInstalled = errors.New("system: release is already installed")
)

const (
	UpdateChannelStable = "stable"
	UpdateChannelBeta   = "beta"
)

const (
	companionAssetName  = "companion.tar.gz"
	companionBundlePath = "companion/companion"
	CompanionDataDir    = "/home/bticino/cfg/extra/companion"
)

var (
	BuildVersion     = "dev"
	BuildGitSHA      = "-"
	BuildReleaseRepo string
)

type BuildInfo struct {
	Version     string `json:"version"`
	GitSHA      string `json:"git_sha"`
	ReleaseRepo string `json:"release_repo,omitempty"`
}

func CurrentBuildInfo() BuildInfo {
	return BuildInfo{Version: BuildVersion, GitSHA: BuildGitSHA, ReleaseRepo: BuildReleaseRepo}
}

const (
	restartDelay   = 250 * time.Millisecond
	restartTimeout = 30 * time.Second
)

type RestartFunc func(context.Context) error

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

type Updater struct {
	source  ReleaseSource
	build   BuildInfo
	policy  func() UpdatePolicy
	restart RestartFunc
	logger  *slog.Logger

	mu     sync.RWMutex
	latest string
	staged string
	err    error
}

func NewUpdater(source ReleaseSource, build BuildInfo, policy func() UpdatePolicy, restart RestartFunc) *Updater {
	return &Updater{source: source, build: build, policy: policy, restart: restart, logger: slog.Default().With("component", "system.update")}
}

func (u *Updater) Status(_ context.Context) (UpdateStatus, error) {
	if u == nil {
		return UpdateStatus{}, ErrUpdateUnavailable
	}

	policy := u.currentPolicy()

	status := UpdateStatus{Enabled: policy.Enabled, Exposed: policy.Exposed, Channel: policy.channel(), CurrentVersion: u.build.Version}
	if strings.TrimSpace(u.build.ReleaseRepo) == "" {
		status.Exposed = false
		status.Stage = "unavailable"

		return status, nil
	}

	u.mu.RLock()
	status.LatestVersion = u.latest

	status.StagedVersion = u.staged
	if u.err != nil {
		status.Error = u.err.Error()
	}

	u.mu.RUnlock()

	if status.LatestVersion == "" {
		status.LatestVersion = status.CurrentVersion
	}

	status.UpdateAvailable = newerVersion(status.LatestVersion, status.CurrentVersion)

	status.RestartRequired = status.StagedVersion != ""
	switch {
	case status.Error != "":
		status.Stage = "failed"
	case status.RestartRequired:
		status.Stage = "staged"
	case status.UpdateAvailable:
		status.Stage = "available"
	default:
		status.Stage = "idle"
	}

	return status, nil
}

func (u *Updater) Check(ctx context.Context) (UpdateStatus, error) {
	if err := u.available(); err != nil {
		return UpdateStatus{}, err
	}

	manifest, err := u.source.Latest(ctx)
	u.mu.Lock()
	if err != nil {
		u.err = err
	} else {
		u.latest = manifest.TagName
		u.err = nil
	}
	u.mu.Unlock()

	if err != nil {
		u.logger.WarnContext(ctx, "update check failed", "error", err)
		return UpdateStatus{}, err
	}

	status, err := u.Status(ctx)
	if err == nil {
		u.logger.DebugContext(ctx, "update check completed", "current_version", status.CurrentVersion, "latest_version", status.LatestVersion, "update_available", status.UpdateAvailable)
	}

	return status, err
}

func (u *Updater) Stage(ctx context.Context) (UpdateStatus, error) {
	if err := u.available(); err != nil {
		return UpdateStatus{}, err
	}

	u.logger.InfoContext(ctx, "update staging started", "current_version", u.build.Version)

	manifest, err := u.source.Latest(ctx)
	if err != nil {
		u.setError(err)
		u.logger.ErrorContext(ctx, "update staging failed", "error", err)

		return UpdateStatus{}, err
	}

	if !newerVersion(manifest.TagName, u.build.Version) {
		u.mu.Lock()
		u.latest, u.err = manifest.TagName, nil
		u.mu.Unlock()

		return u.Status(ctx)
	}

	if err := u.stageManifest(ctx, manifest); err != nil {
		return UpdateStatus{}, err
	}

	u.mu.Lock()
	u.latest = manifest.TagName
	u.mu.Unlock()

	return u.Status(ctx)
}

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

func (u *Updater) available() error {
	if u == nil || u.source == nil || strings.TrimSpace(u.build.ReleaseRepo) == "" || !u.currentPolicy().Enabled {
		return ErrUpdateUnavailable
	}

	return nil
}

func (u *Updater) currentPolicy() UpdatePolicy {
	if u.policy == nil {
		return UpdatePolicy{}
	}

	return u.policy()
}

func (u *Updater) setError(err error) {
	u.mu.Lock()
	u.err = err
	u.mu.Unlock()
}

func stageArchive(ctx context.Context, source io.Reader, digest, dataDir string) error {
	if strings.TrimSpace(dataDir) == "" {
		return ErrUpdateUnavailable
	}

	expected, err := parseSHA256Digest(digest)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("create update directory: %w", err)
	}

	temporary, err := os.CreateTemp("", "companion-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create update archive: %w", err)
	}

	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck // temporary archive cleanup

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temporary, hash), source); err != nil {
		if closeErr := temporary.Close(); closeErr != nil {
			return fmt.Errorf("write update archive: %w", errors.Join(err, closeErr))
		}

		return fmt.Errorf("write update archive: %w", err)
	}

	if err := temporary.Sync(); err != nil {
		if closeErr := temporary.Close(); closeErr != nil {
			return fmt.Errorf("sync update archive: %w", errors.Join(err, closeErr))
		}

		return fmt.Errorf("sync update archive: %w", err)
	}

	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close update archive: %w", err)
	}

	if !bytes.Equal(hash.Sum(nil), expected) {
		return ErrArtifactDigestMismatch
	}

	stagingDir, err := os.MkdirTemp(dataDir, ".companion-update-*")
	if err != nil {
		return fmt.Errorf("create update staging directory: %w", err)
	}
	defer os.RemoveAll(stagingDir) //nolint:errcheck // temporary staging cleanup

	if err := exec.CommandContext(ctx, "/bin/tar", "-xzf", temporaryPath, "-C", stagingDir, companionBundlePath).Run(); err != nil { // #nosec G204 -- fixed tar executable; archive and staging paths are locally created.
		return fmt.Errorf("extract update archive: %w", err)
	}

	binaryPath := filepath.Join(stagingDir, companionBundlePath)

	binary, err := os.Open(binaryPath)
	if err != nil {
		return fmt.Errorf("open extracted companion: %w", err)
	}

	if err := binary.Chmod(0o755); err != nil {
		if closeErr := binary.Close(); closeErr != nil {
			return fmt.Errorf("chmod extracted companion: %w", errors.Join(err, closeErr))
		}

		return fmt.Errorf("chmod extracted companion: %w", err)
	}

	if err := binary.Sync(); err != nil {
		if closeErr := binary.Close(); closeErr != nil {
			return fmt.Errorf("sync extracted companion: %w", errors.Join(err, closeErr))
		}

		return fmt.Errorf("sync extracted companion: %w", err)
	}

	if err := binary.Close(); err != nil {
		return fmt.Errorf("close extracted companion: %w", err)
	}

	if err := os.Rename(binaryPath, filepath.Join(dataDir, "companion.new")); err != nil {
		return fmt.Errorf("stage binary: %w", err)
	}

	return nil
}

var semverPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z.-]+)?$`)

func newerVersion(candidate, current string) bool {
	candidateParts := semverPattern.FindStringSubmatch(candidate)

	currentParts := semverPattern.FindStringSubmatch(current)
	if candidateParts == nil || currentParts == nil {
		return false
	}

	for i := 1; i <= 3; i++ {
		if len(candidateParts[i]) != len(currentParts[i]) {
			return len(candidateParts[i]) > len(currentParts[i])
		}

		if candidateParts[i] != currentParts[i] {
			return candidateParts[i] > currentParts[i]
		}
	}

	return comparePrerelease(candidateParts[4], currentParts[4]) > 0
}

func comparePrerelease(candidate, current string) int {
	if candidate == current {
		return 0
	}

	if candidate == "" {
		return 1
	}

	if current == "" {
		return -1
	}

	left, right := strings.Split(candidate, "."), strings.Split(current, ".")
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] == right[i] {
			continue
		}

		leftNumber, rightNumber := isNumeric(left[i]), isNumeric(right[i])
		if leftNumber != rightNumber {
			if leftNumber {
				return -1
			}

			return 1
		}

		if leftNumber && len(left[i]) != len(right[i]) {
			if len(left[i]) > len(right[i]) {
				return 1
			}

			return -1
		}

		if left[i] > right[i] {
			return 1
		}

		return -1
	}

	if len(left) > len(right) {
		return 1
	}

	return -1
}

func isNumeric(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}

	return value != ""
}

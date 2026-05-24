package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bticino-go-companion/internal/config"
	"golang.org/x/mod/semver"
)

const (
	StageIdle       = "idle"
	StageChecking   = "checking"
	StageAvailable  = "available"
	StageApplying   = "applying"
	StageRestarting = "restarting"
	StageHealthy    = "healthy"
	StageRollback   = "rollback"
	StageFailed     = "failed"

	updateUserAgent = "bticino-go-companion-updater"
)

var (
	ErrNoAvailableUpdate = errors.New("no available update metadata")
	ErrMissingArtifact   = errors.New("artifact_path is required")
	ErrNoPreviousBinary  = errors.New("no previous binary available")
)

type Artifact struct {
	Version string `json:"version,omitempty"`
	Path    string `json:"artifact_path"`
	SHA256  string `json:"sha256,omitempty"`
}

type Status struct {
	CurrentVersion  string    `json:"current_version"`
	Stage           string    `json:"stage"`
	Available       *Artifact `json:"available,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	LastCheckedAt   string    `json:"last_checked_at,omitempty"`
	LastAppliedAt   string    `json:"last_applied_at,omitempty"`
	LastRollbackAt  string    `json:"last_rollback_at,omitempty"`
	CanRollback     bool      `json:"can_rollback"`
	RestartRequired bool      `json:"restart_required"`
}

type checkManifest struct {
	AvailableVersion string `json:"available_version"`
	ArtifactPath     string `json:"artifact_path"`
	SHA256           string `json:"sha256"`
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		Digest             string `json:"digest"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

type Manager struct {
	mu sync.RWMutex

	cfg      config.Config
	logger   *log.Logger
	healthFn func(context.Context) error
	restart  func() error
	now      func() time.Time
	http     *http.Client

	status            Status
	rollbackToVersion string
}

func NewManager(cfg config.Config, logger *log.Logger, healthFn func(context.Context) error) *Manager {
	m := &Manager{
		cfg:      cfg,
		logger:   logger,
		healthFn: healthFn,
		now: func() time.Time {
			return time.Now().UTC()
		},
		http: &http.Client{Timeout: 20 * time.Second},
		status: Status{
			CurrentVersion: cfg.Version,
			Stage:          StageIdle,
		},
	}
	m.restart = func() error {
		if strings.TrimSpace(cfg.UpdateServiceScript) == "" {
			return errors.New("update service restart script is empty")
		}
		cmd := exec.Command(cfg.UpdateServiceScript, "restart")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("service restart failed: %w output=%s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return m
}

func (m *Manager) SetRestartForTest(fn func() error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restart = fn
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cpy := m.status
	if cpy.Available != nil {
		a := *cpy.Available
		cpy.Available = &a
	}
	return cpy
}

func (m *Manager) Check(override *Artifact) (Status, error) {
	m.mu.Lock()
	m.setStageLocked(StageChecking, "")
	m.status.LastCheckedAt = m.now().Format(time.RFC3339)
	m.mu.Unlock()

	var cand Artifact
	var found bool
	var err error
	if override != nil {
		cand = *override
		found = true
	} else {
		cand, found, err = m.readManifest()
		if err != nil {
			m.mu.Lock()
			m.setStageLocked(StageFailed, err.Error())
			out := m.status
			m.mu.Unlock()
			return out, err
		}
		if !found {
			cand, found, err = m.readGitHubRelease()
			if err != nil {
				m.mu.Lock()
				m.setStageLocked(StageFailed, err.Error())
				out := m.status
				m.mu.Unlock()
				return out, err
			}
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	currentVersion := firstNonEmpty(m.status.CurrentVersion, m.cfg.Version)
	if !found || strings.TrimSpace(cand.Version) == "" || !isNewerVersion(cand.Version, currentVersion) {
		m.status.Available = nil
		m.status.RestartRequired = false
		m.setStageLocked(StageHealthy, "")
		return m.status, nil
	}
	if strings.TrimSpace(cand.Path) == "" {
		m.setStageLocked(StageFailed, ErrMissingArtifact.Error())
		return m.status, ErrMissingArtifact
	}
	m.status.Available = &cand
	m.status.RestartRequired = false
	m.setStageLocked(StageAvailable, "")
	return m.status, nil
}

func (m *Manager) Apply(override *Artifact) (Status, error) {
	m.mu.Lock()
	var candidate Artifact
	switch {
	case override != nil:
		candidate = *override
	case m.status.Available != nil:
		candidate = *m.status.Available
	default:
		m.mu.Unlock()
		return m.Status(), ErrNoAvailableUpdate
	}
	m.setStageLocked(StageApplying, "")
	m.status.RestartRequired = true
	m.mu.Unlock()

	if err := m.applyBinary(candidate); err != nil {
		m.mu.Lock()
		m.setStageLocked(StageFailed, err.Error())
		out := m.status
		m.mu.Unlock()
		return out, err
	}

	m.mu.Lock()
	m.rollbackToVersion = firstNonEmpty(m.status.CurrentVersion, m.cfg.Version)
	m.status.CurrentVersion = firstNonEmpty(candidate.Version, m.status.CurrentVersion)
	m.status.Available = nil
	m.status.LastAppliedAt = m.now().Format(time.RFC3339)
	m.mu.Unlock()

	m.mu.Lock()
	m.setStageLocked(StageRestarting, "")
	m.mu.Unlock()

	if err := m.restart(); err != nil {
		_, _ = m.rollbackInternal("restart failed: " + err.Error())
		m.mu.Lock()
		m.setStageLocked(StageFailed, err.Error())
		out := m.status
		m.mu.Unlock()
		return out, err
	}

	if err := m.healthWindowCheck(); err != nil {
		_, _ = m.rollbackInternal("health check failed: " + err.Error())
		m.mu.Lock()
		m.setStageLocked(StageFailed, err.Error())
		out := m.status
		m.mu.Unlock()
		return out, err
	}

	m.mu.Lock()
	m.status.RestartRequired = false
	m.setStageLocked(StageHealthy, "")
	out := m.status
	m.mu.Unlock()
	return out, nil
}

func (m *Manager) Rollback() (Status, error) {
	status, err := m.rollbackInternal("")
	if err != nil {
		m.mu.Lock()
		m.setStageLocked(StageFailed, err.Error())
		out := m.status
		m.mu.Unlock()
		return out, err
	}
	return status, nil
}

func (m *Manager) rollbackInternal(reason string) (Status, error) {
	m.mu.Lock()
	m.setStageLocked(StageRollback, reason)
	m.status.RestartRequired = true
	m.mu.Unlock()

	prev := m.cfg.UpdateBinPreviousPath()
	current := m.cfg.UpdateBinCurrentPath()

	if _, err := os.Stat(prev); err != nil {
		return m.Status(), ErrNoPreviousBinary
	}
	if err := copyFile(prev, current, 0o755); err != nil {
		return m.Status(), fmt.Errorf("restore previous binary: %w", err)
	}

	if err := m.restart(); err != nil {
		return m.Status(), err
	}
	if err := m.healthWindowCheck(); err != nil {
		return m.Status(), err
	}

	m.mu.Lock()
	m.status.CurrentVersion = firstNonEmpty(m.rollbackToVersion, m.cfg.Version, m.status.CurrentVersion)
	m.status.LastRollbackAt = m.now().Format(time.RFC3339)
	m.status.RestartRequired = false
	m.setStageLocked(StageHealthy, "")
	out := m.status
	m.mu.Unlock()
	return out, nil
}

func (m *Manager) applyBinary(candidate Artifact) error {
	if strings.TrimSpace(candidate.Path) == "" {
		return ErrMissingArtifact
	}
	archivePath, cleanup, err := m.resolveArtifact(candidate.Path)
	if err != nil {
		return err
	}
	defer cleanup()

	if _, err := os.Stat(archivePath); err != nil {
		return fmt.Errorf("candidate artifact not found: %w", err)
	}
	if err := verifySHA256(archivePath, candidate.SHA256); err != nil {
		return err
	}
	candidatePath, cleanupCandidate, err := extractCompanionBinary(archivePath)
	if err != nil {
		return err
	}
	defer cleanupCandidate()

	current := m.cfg.UpdateBinCurrentPath()
	prev := m.cfg.UpdateBinPreviousPath()
	tmpCandidate := m.cfg.UpdateBinCandidatePath() + ".tmp"
	candidateFinal := m.cfg.UpdateBinCandidatePath()

	for _, p := range []string{current, prev, tmpCandidate, candidateFinal} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
	}

	if err := copyFile(candidatePath, tmpCandidate, 0o755); err != nil {
		return fmt.Errorf("copy candidate: %w", err)
	}
	if err := os.Rename(tmpCandidate, candidateFinal); err != nil {
		return fmt.Errorf("promote candidate: %w", err)
	}
	if _, err := os.Stat(current); err == nil {
		if err := copyFile(current, prev, 0o755); err != nil {
			return fmt.Errorf("copy previous: %w", err)
		}
	}
	if err := copyFile(candidateFinal, current, 0o755); err != nil {
		return fmt.Errorf("activate candidate: %w", err)
	}
	return nil
}

func (m *Manager) resolveArtifact(path string) (string, func(), error) {
	trimmed := strings.TrimSpace(path)
	switch {
	case strings.HasPrefix(trimmed, "https://"), strings.HasPrefix(trimmed, "http://"):
		tmp := filepath.Join(os.TempDir(), fmt.Sprintf("bticino-go-companion-artifact-%d.tar.gz", m.now().UnixNano()))
		if err := m.downloadFile(trimmed, tmp); err != nil {
			return "", func() {}, fmt.Errorf("download artifact: %w", err)
		}
		return tmp, func() { _ = os.Remove(tmp) }, nil
	default:
		return trimmed, func() {}, nil
	}
}

func (m *Manager) readManifest() (Artifact, bool, error) {
	path := strings.TrimSpace(m.cfg.UpdateManifestPath)
	if path == "" {
		return Artifact{}, false, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Artifact{}, false, fmt.Errorf("read update manifest: %w", err)
	}
	var mf checkManifest
	if err := json.Unmarshal(b, &mf); err != nil {
		return Artifact{}, false, fmt.Errorf("parse update manifest: %w", err)
	}
	if strings.TrimSpace(mf.AvailableVersion) == "" || strings.TrimSpace(mf.ArtifactPath) == "" {
		return Artifact{}, false, nil
	}
	return Artifact{
		Version: strings.TrimSpace(mf.AvailableVersion),
		Path:    strings.TrimSpace(mf.ArtifactPath),
		SHA256:  strings.TrimSpace(strings.ToLower(mf.SHA256)),
	}, true, nil
}

func (m *Manager) readGitHubRelease() (Artifact, bool, error) {
	repo := strings.TrimSpace(m.cfg.UpdateReleaseRepo)
	if repo == "" {
		return Artifact{}, false, nil
	}

	apiBase := strings.TrimSpace(m.cfg.UpdateReleaseAPI)
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	assetName := strings.TrimSpace(m.cfg.UpdateReleaseAsset)
	if assetName == "" {
		assetName = "companion"
	}

	url := strings.TrimRight(apiBase, "/") + "/repos/" + repo + "/releases/latest"
	resp, err := m.doGET(url, "application/vnd.github+json")
	if err != nil {
		return Artifact{}, false, fmt.Errorf("github release query failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Artifact{}, false, httpStatusError("github release query", resp)
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Artifact{}, false, fmt.Errorf("decode github release: %w", err)
	}
	tag := strings.TrimSpace(rel.TagName)
	if tag == "" {
		return Artifact{}, false, nil
	}

	artifactURL := ""
	artifactDigest := ""
	for _, asset := range rel.Assets {
		name := strings.TrimSpace(asset.Name)
		if name == assetName {
			artifactURL = strings.TrimSpace(asset.BrowserDownloadURL)
			artifactDigest = strings.TrimSpace(asset.Digest)
			break
		}
	}
	if artifactURL == "" {
		return Artifact{}, false, fmt.Errorf("github release missing asset %q", assetName)
	}
	checksum, err := parseAssetDigest(artifactDigest)
	if err != nil {
		return Artifact{}, false, fmt.Errorf("github release invalid digest for asset %q: %w", assetName, err)
	}

	return Artifact{
		Version: tag,
		Path:    artifactURL,
		SHA256:  checksum,
	}, true, nil
}

func (m *Manager) downloadFile(url string, dst string) error {
	resp, err := m.doGET(url, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpStatusError("download file", resp)
	}

	tmp := dst + ".tmp"
	cleanup := func() { _ = os.Remove(tmp) }
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		_ = out.Close()
		cleanup()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		cleanup()
		return err
	}
	if err := out.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		cleanup()
		return err
	}
	return nil
}

func parseAssetDigest(raw string) (string, error) {
	digest := strings.ToLower(strings.TrimSpace(raw))
	if digest == "" {
		return "", errors.New("empty digest")
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		return "", errors.New("unsupported digest format")
	}
	sum := strings.TrimSpace(strings.TrimPrefix(digest, prefix))
	if len(sum) != 64 || !isLowerHex(sum) {
		return "", errors.New("invalid sha256 digest")
	}
	return sum, nil
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

func isNewerVersion(candidate string, current string) bool {
	candidate = strings.TrimSpace(candidate)
	current = strings.TrimSpace(current)
	if candidate == "" {
		return false
	}

	candidateSem := normalizeSemver(candidate)
	currentSem := normalizeSemver(current)
	if semver.IsValid(candidateSem) && semver.IsValid(currentSem) {
		return semver.Compare(candidateSem, currentSem) > 0
	}

	return candidate != current
}

func normalizeSemver(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

func (m *Manager) healthWindowCheck() error {
	if m.healthFn == nil {
		return nil
	}
	timeout := time.Duration(max(1, m.cfg.UpdateHealthTimeoutSec)) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return m.healthFn(ctx)
}

func (m *Manager) setStageLocked(stage string, msg string) {
	m.status.Stage = stage
	m.status.LastError = strings.TrimSpace(msg)
	m.status.CanRollback = hasFile(m.cfg.UpdateBinPreviousPath())
	if m.logger != nil {
		m.logger.Printf("update stage=%s err=%s restart_required=%t", stage, m.status.LastError, m.status.RestartRequired)
	}
}

func verifySHA256(path string, expected string) error {
	expected = strings.TrimSpace(strings.ToLower(expected))
	if expected == "" {
		return nil
	}
	if len(expected) != 64 || !isLowerHex(expected) {
		return fmt.Errorf("invalid expected artifact checksum format")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open artifact for checksum: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("compute artifact checksum: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return fmt.Errorf("artifact checksum mismatch expected=%s actual=%s", expected, actual)
	}
	return nil
}

func extractCompanionBinary(archivePath string) (string, func(), error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", func() {}, fmt.Errorf("open artifact archive: %w", err)
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return "", func() {}, fmt.Errorf("open gzip archive: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	tmpDir, err := os.MkdirTemp("", "bticino-go-companion-candidate-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	dst := filepath.Join(tmpDir, "companion")

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("read tar archive: %w", err)
		}
		if hdr == nil || hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.ToSlash(strings.TrimSpace(hdr.Name))
		if name != "companion/companion" {
			continue
		}

		out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("create extracted candidate: %w", err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			cleanup()
			return "", func() {}, fmt.Errorf("extract companion binary: %w", err)
		}
		if err := out.Close(); err != nil {
			cleanup()
			return "", func() {}, err
		}
		if err := os.Chmod(dst, 0o755); err != nil {
			cleanup()
			return "", func() {}, err
		}
		return dst, cleanup, nil
	}

	cleanup()
	return "", func() {}, errors.New("companion binary not found in artifact archive")
}

func copyFile(src string, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	cleanup := func() { _ = os.Remove(tmp) }
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		cleanup()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		cleanup()
		return err
	}
	if err := out.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		cleanup()
		return err
	}
	return nil
}

func firstNonEmpty(items ...string) string {
	for _, item := range items {
		if strings.TrimSpace(item) != "" {
			return strings.TrimSpace(item)
		}
	}
	return ""
}

func hasFile(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func max(a int, b int) int {
	if a > b {
		return a
	}
	return b
}

func (m *Manager) doGET(url string, accept string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(accept) != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", updateUserAgent)
	return m.http.Do(req)
}

func httpStatusError(operation string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	prefix := strings.TrimSpace(operation)
	if prefix == "" {
		prefix = "http request"
	}
	return fmt.Errorf("%s status=%d body=%s", prefix, resp.StatusCode, strings.TrimSpace(string(body)))
}

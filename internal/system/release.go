package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrInvalidReleaseManifest = errors.New("system: invalid release manifest")
	ErrReleaseAssetNotFound   = errors.New("system: release asset not found")
	ErrInvalidArtifactDigest  = errors.New("system: invalid artifact digest")
	ErrArtifactDigestMismatch = errors.New("system: artifact digest mismatch")
)

type ReleaseAsset struct {
	Name        string `json:"name"`
	Digest      string `json:"digest"`
	DownloadURL string `json:"browser_download_url"`
}

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

func (m ReleaseManifest) Asset(name string) (ReleaseAsset, error) {
	for _, asset := range m.Assets {
		if asset.Name == name {
			return asset, nil
		}
	}

	return ReleaseAsset{}, ErrReleaseAssetNotFound
}

func VerifySHA256(reader io.Reader, digest string) error {
	expected, err := parseSHA256Digest(digest)
	if err != nil {
		return err
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return fmt.Errorf("hash artifact: %w", err)
	}

	if !bytes.Equal(hash.Sum(nil), expected) {
		return ErrArtifactDigestMismatch
	}

	return nil
}

func parseSHA256Digest(digest string) ([]byte, error) {
	value, ok := strings.CutPrefix(strings.ToLower(digest), "sha256:")
	if !ok || len(value) != sha256.Size*2 {
		return nil, ErrInvalidArtifactDigest
	}

	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return nil, ErrInvalidArtifactDigest
	}

	return decoded, nil
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

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

func (c *GitHubReleaseClient) Download(ctx context.Context, asset ReleaseAsset) (io.ReadCloser, error) {
	if c == nil || c.client == nil {
		return nil, ErrRuntimeUnavailable
	}

	parsedURL, err := url.Parse(asset.DownloadURL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return nil, ErrInvalidReleaseManifest
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.DownloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create artifact request: %w", err)
	}

	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request artifact: %w", err)
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		closeErr := response.Body.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("artifact response: %s: close response: %w", response.Status, closeErr)
		}

		return nil, fmt.Errorf("artifact response: %s", response.Status)
	}

	return response.Body, nil
}

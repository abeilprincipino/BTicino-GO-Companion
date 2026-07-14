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
	TagName string         `json:"tag_name"`
	Assets  []ReleaseAsset `json:"assets"`
}

func ParseGitHubReleaseManifest(reader io.Reader) (ReleaseManifest, error) {
	var manifest ReleaseManifest
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&manifest); err != nil {
		return ReleaseManifest{}, fmt.Errorf("decode release manifest: %w", ErrInvalidReleaseManifest)
	}
	if decoder.Decode(&struct{}{}) != io.EOF || manifest.TagName == "" {
		return ReleaseManifest{}, ErrInvalidReleaseManifest
	}
	for _, asset := range manifest.Assets {
		if asset.Name == "" || asset.Digest == "" || asset.DownloadURL == "" {
			return ReleaseManifest{}, ErrInvalidReleaseManifest
		}
		if _, err := parseSHA256Digest(asset.Digest); err != nil {
			return ReleaseManifest{}, fmt.Errorf("asset %q: %w", asset.Name, err)
		}
		parsedURL, err := url.Parse(asset.DownloadURL)
		if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" {
			return ReleaseManifest{}, fmt.Errorf("asset %q: %w", asset.Name, ErrInvalidReleaseManifest)
		}
	}
	return manifest, nil
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
	client HTTPDoer
	url    string
}

func NewGitHubReleaseClient(client HTTPDoer, latestReleaseURL string) *GitHubReleaseClient {
	return &GitHubReleaseClient{client: client, url: latestReleaseURL}
}

func (c *GitHubReleaseClient) Latest(ctx context.Context) (ReleaseManifest, error) {
	if c == nil || c.client == nil || c.url == "" {
		return ReleaseManifest{}, ErrRuntimeUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return ReleaseManifest{}, fmt.Errorf("create latest release request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := c.client.Do(request)
	if err != nil {
		return ReleaseManifest{}, fmt.Errorf("request latest release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return ReleaseManifest{}, fmt.Errorf("latest release response: %s", response.Status)
	}
	return ParseGitHubReleaseManifest(io.LimitReader(response.Body, 1<<20))
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

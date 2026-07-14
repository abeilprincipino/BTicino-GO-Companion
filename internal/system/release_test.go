package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestParseGitHubReleaseManifest(t *testing.T) {
	manifest, err := ParseGitHubReleaseManifest(strings.NewReader(`{"tag_name":"v1.2.3","assets":[{"name":"companion.tar.gz","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","browser_download_url":"https://github.com/example/companion.tar.gz"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	asset, err := manifest.Asset("companion.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.TagName != "v1.2.3" || asset.Name != "companion.tar.gz" {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestParseGitHubReleaseManifest_RejectsInvalidAssets(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"missing tag", `{"assets":[]}`},
		{"missing digest", `{"tag_name":"v1","assets":[{"name":"companion","browser_download_url":"https://example.test/a"}]}`},
		{"invalid digest", `{"tag_name":"v1","assets":[{"name":"companion","digest":"sha256:bad","browser_download_url":"https://example.test/a"}]}`},
		{"insecure url", `{"tag_name":"v1","assets":[{"name":"companion","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","browser_download_url":"http://example.test/a"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseGitHubReleaseManifest(strings.NewReader(test.body))
			if !errors.Is(err, ErrInvalidReleaseManifest) && !errors.Is(err, ErrInvalidArtifactDigest) {
				t.Fatalf("ParseGitHubReleaseManifest() error = %v", err)
			}
		})
	}
}

func TestVerifySHA256(t *testing.T) {
	data := []byte("verified artifact")
	digest := sha256.Sum256(data)
	expected := "sha256:" + hex.EncodeToString(digest[:])
	if err := VerifySHA256(bytes.NewReader(data), expected); err != nil {
		t.Fatal(err)
	}
	if err := VerifySHA256(bytes.NewReader([]byte("tampered artifact")), expected); !errors.Is(err, ErrArtifactDigestMismatch) {
		t.Fatalf("VerifySHA256() error = %v", err)
	}
	if err := VerifySHA256(bytes.NewReader(data), "sha256:bad"); !errors.Is(err, ErrInvalidArtifactDigest) {
		t.Fatalf("VerifySHA256() error = %v", err)
	}
}

func TestGitHubReleaseClient(t *testing.T) {
	client := &testHTTPClient{responses: []*http.Response{
		response(http.StatusOK, `{"tag_name":"v1.2.3","assets":[{"name":"companion.tar.gz","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","browser_download_url":"https://downloads.example.test/companion.tar.gz"}]}`),
		response(http.StatusOK, "artifact"),
	}}
	releases := NewGitHubReleaseClient(client, "https://api.github.com/repos/example/companion/releases/latest")
	manifest, err := releases.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	asset, err := manifest.Asset("companion.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := releases.Download(context.Background(), asset)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(artifact)
	closeErr := artifact.Close()
	if readErr != nil || closeErr != nil || string(data) != "artifact" {
		t.Fatalf("artifact = %q, read error = %v, close error = %v", data, readErr, closeErr)
	}
	if client.requests[0].Header.Get("Accept") != "application/vnd.github+json" || client.requests[1].URL.String() != asset.DownloadURL {
		t.Fatalf("requests = %#v", client.requests)
	}
}

type testHTTPClient struct {
	responses []*http.Response
	requests  []*http.Request
}

func (c *testHTTPClient) Do(request *http.Request) (*http.Response, error) {
	c.requests = append(c.requests, request)
	if len(c.responses) == 0 {
		return nil, errors.New("unexpected request")
	}
	response := c.responses[0]
	c.responses = c.responses[1:]
	return response, nil
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body))}
}

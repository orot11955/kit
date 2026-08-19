package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"kit/internal/buildinfo"
)

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		current string
		latest  string
		want    int
	}{
		{"v1.2.3", "v1.2.4", -1},
		{"v1.2.3", "v1.2.3", 0},
		{"v2.0.0", "v1.9.9", 1},
	}
	for _, tt := range tests {
		got, err := CompareSemver(tt.current, tt.latest)
		if err != nil || got != tt.want {
			t.Fatalf("CompareSemver(%q, %q) = %d, %v; want %d", tt.current, tt.latest, got, err, tt.want)
		}
	}
	for _, invalid := range []string{"dev", "1.2.3", "v1.2", "v01.2.3", "v1.2.x"} {
		if _, err := CompareSemver(invalid, "v1.2.3"); err == nil {
			t.Fatalf("expected %q to be rejected", invalid)
		}
	}
}

func TestValidateReleaseRequiresBuildToMatchCommit(t *testing.T) {
	release := Release{
		SchemaVersion: 1,
		Version:       "v1.2.3",
		Build:         "bbbbbbbb",
		Commit:        strings.Repeat("a", 40),
		PublishedAt:   "2026-08-15T00:00:00Z",
		Downloads: map[string]Download{
			"darwin-arm64": {URL: "/kit", SHA256: strings.Repeat("0", 64)},
		},
	}
	if err := validateRelease(release); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected build/commit mismatch, got %v", err)
	}
}

func TestRequireSecureResponseRejectsHTTPRedirectTarget(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "http://downloads.example.invalid/kit", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := &http.Response{Request: request}
	expected, err := url.Parse("https://downloads.example.invalid/kit")
	if err != nil {
		t.Fatal(err)
	}
	if err := requireSecureResponse(response, expected, false); err == nil {
		t.Fatal("expected an HTTP redirect target to be rejected")
	}
	httpExpected, err := url.Parse("http://downloads.example.invalid/kit")
	if err != nil {
		t.Fatal(err)
	}
	if err := requireSecureResponse(response, httpExpected, true); err != nil {
		t.Fatalf("same-origin test-only HTTP should be allowed: %v", err)
	}
	otherRequest, err := http.NewRequest(http.MethodGet, "https://mirror.example.invalid/kit", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireSecureResponse(&http.Response{Request: otherRequest}, expected, false); err == nil {
		t.Fatal("expected a cross-origin redirect target to be rejected")
	}
}

func TestRunAlreadyLatestDoesNotTouchExecutable(t *testing.T) {
	client := releaseClient(t, []byte("new"), "v1.2.3", "")
	result, err := Run(context.Background(), Config{
		ReleaseURL: "http://kit.test/release.json",
		HTTPClient: client,
		AllowHTTP:  true,
		Current: buildinfo.Info{
			Version: "v1.2.3",
		},
		Executable:   filepath.Join(t.TempDir(), "missing"),
		ExpectedPath: filepath.Join(t.TempDir(), "different"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated || result.Latest != "v1.2.3" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestRunVerifiesAndAtomicallyReplaces(t *testing.T) {
	asset := []byte("new binary")
	client := releaseClient(t, asset, "v1.2.4", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "kit")
	if err := os.WriteFile(path, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Config{
		ReleaseURL:   "http://kit.test/release.json",
		HTTPClient:   client,
		AllowHTTP:    true,
		Current:      buildinfo.Info{Version: "v1.2.3"},
		Executable:   path,
		ExpectedPath: path,
		RunVersion: func(context.Context, string) (buildinfo.Info, error) {
			return buildinfo.Info{Version: "v1.2.4", Commit: strings.Repeat("a", 40), BuildDate: "2026-08-15T00:00:00Z", Target: runtime.GOOS + "/" + runtime.GOARCH}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated {
		t.Fatalf("expected update: %#v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(asset) {
		t.Fatalf("executable was not replaced: %q", got)
	}
}

func TestRunChecksumFailurePreservesExecutable(t *testing.T) {
	client := releaseClient(t, []byte("untrusted"), "v1.2.4", strings.Repeat("0", 64))
	dir := t.TempDir()
	path := filepath.Join(dir, "kit")
	if err := os.WriteFile(path, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Config{
		ReleaseURL: "http://kit.test/release.json", HTTPClient: client, AllowHTTP: true,
		Current: buildinfo.Info{Version: "v1.2.3"}, Executable: path, ExpectedPath: path,
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum error, got %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != "old binary" {
		t.Fatalf("old executable changed: %q, %v", got, readErr)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func releaseClient(t *testing.T, asset []byte, version, checksumOverride string) *http.Client {
	t.Helper()
	digest := sha256.Sum256(asset)
	checksum := hex.EncodeToString(digest[:])
	if checksumOverride != "" {
		checksum = checksumOverride
	}
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body []byte
		switch r.URL.Path {
		case "/release.json":
			key, err := targetKey(runtime.GOOS, runtime.GOARCH)
			if err != nil {
				t.Fatal(err)
			}
			body, _ = json.Marshal(Release{
				SchemaVersion: 1,
				Version:       version,
				Build:         "aaaaaaaa",
				Commit:        strings.Repeat("a", 40),
				PublishedAt:   "2026-08-15T00:00:00Z",
				Downloads:     map[string]Download{key: {URL: "http://kit.test/kit", SHA256: checksum}},
			})
		case "/kit":
			body = asset
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header), Request: r}, nil
	})}
}

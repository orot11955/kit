package update

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"kit/internal/buildinfo"
)

func TestRunCheckOnlyDoesNotDownloadAssetOrInspectInstallPath(t *testing.T) {
	requests := []string{}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.Path)
		if r.URL.Path != "/release.json" {
			t.Fatalf("check-only unexpectedly requested %s", r.URL.Path)
		}
		body := `{"schema_version":1,"version":"v1.2.4","build":"aaaaaaaa","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","published_at":"2026-08-15T00:00:00Z","downloads":{"linux-amd64":{"url":"http://kit.test/kit","sha256":"0000000000000000000000000000000000000000000000000000000000000000"}}}`
		if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
			body = strings.Replace(body, "linux-amd64", "darwin-arm64", 1)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	result, err := Run(context.Background(), Config{
		ReleaseURL:   "http://kit.test/release.json",
		HTTPClient:   client,
		AllowHTTP:    true,
		CheckOnly:    true,
		Current:      buildinfo.Info{Version: "v1.2.3"},
		Executable:   filepath.Join(t.TempDir(), "missing-current"),
		ExpectedPath: filepath.Join(t.TempDir(), "missing-expected"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated || !result.UpdateAvailable || result.Latest != "v1.2.4" {
		t.Fatalf("unexpected check result: %#v", result)
	}
	if len(requests) != 1 || requests[0] != "/release.json" {
		t.Fatalf("unexpected requests: %#v", requests)
	}
}

func TestRunUpdatePreservesPreviousBinary(t *testing.T) {
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
		Current:      buildinfo.Info{Version: "v1.2.3", Commit: strings.Repeat("b", 40), BuildDate: "2026-08-14T00:00:00Z", Target: runtime.GOOS + "/" + runtime.GOARCH},
		Executable:   path,
		ExpectedPath: path,
		RunVersion: func(_ context.Context, candidate string) (buildinfo.Info, error) {
			data, err := os.ReadFile(candidate)
			if err != nil {
				return buildinfo.Info{}, err
			}
			if string(data) != string(asset) {
				return buildinfo.Info{}, errors.New("unexpected candidate")
			}
			return buildinfo.Info{Version: "v1.2.4", Commit: strings.Repeat("a", 40), BuildDate: "2026-08-15T00:00:00Z", Target: runtime.GOOS + "/" + runtime.GOARCH}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.Previous != "v1.2.3" {
		t.Fatalf("unexpected update result: %#v", result)
	}
	previous, err := os.ReadFile(path + ".previous")
	if err != nil {
		t.Fatal(err)
	}
	if string(previous) != "old binary" {
		t.Fatalf("previous binary was not preserved: %q", previous)
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != string(asset) {
		t.Fatalf("current binary was not updated: %q %v", current, err)
	}
}

func TestRunRollbackSwapsCurrentAndPreviousWithoutNetwork(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kit")
	previousPath := path + ".previous"
	if err := os.WriteFile(path, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previousPath, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network must not be used by rollback")
	})}
	versionFor := func(_ context.Context, candidate string) (buildinfo.Info, error) {
		data, err := os.ReadFile(candidate)
		if err != nil {
			return buildinfo.Info{}, err
		}
		switch string(data) {
		case "old binary":
			return buildinfo.Info{Version: "v1.2.3", Commit: strings.Repeat("b", 40), BuildDate: "2026-08-14T00:00:00Z", Target: runtime.GOOS + "/" + runtime.GOARCH}, nil
		case "new binary":
			return buildinfo.Info{Version: "v1.2.4", Commit: strings.Repeat("a", 40), BuildDate: "2026-08-15T00:00:00Z", Target: runtime.GOOS + "/" + runtime.GOARCH}, nil
		default:
			return buildinfo.Info{}, errors.New("unknown binary")
		}
	}
	result, err := Run(context.Background(), Config{
		HTTPClient:   client,
		Rollback:     true,
		Current:      buildinfo.Info{Version: "v1.2.4", Commit: strings.Repeat("a", 40), BuildDate: "2026-08-15T00:00:00Z", Target: runtime.GOOS + "/" + runtime.GOARCH},
		Executable:   path,
		ExpectedPath: path,
		RunVersion:   versionFor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.RolledBack || result.Previous != "v1.2.3" || result.Current != "v1.2.4" {
		t.Fatalf("unexpected rollback result: %#v", result)
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != "old binary" {
		t.Fatalf("rollback did not activate previous binary: %q %v", current, err)
	}
	previous, err := os.ReadFile(previousPath)
	if err != nil || string(previous) != "new binary" {
		t.Fatalf("rollback did not rotate current binary into previous slot: %q %v", previous, err)
	}
}

func TestRunRollbackRejectsSymlinkPrevious(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kit")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("current"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path+".previous"); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Config{
		Rollback:     true,
		Current:      buildinfo.Info{Version: "v1.2.4", Commit: strings.Repeat("a", 40)},
		Executable:   path,
		ExpectedPath: path,
		RunVersion: func(context.Context, string) (buildinfo.Info, error) {
			return buildinfo.Info{}, errors.New("must not execute symlink previous")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "regular non-symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

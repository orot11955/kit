package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"kit/internal/buildinfo"
)

const defaultReleaseURL = "https://kit.2juho.com/release.json"

type Download struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type Release struct {
	SchemaVersion int                 `json:"schema_version"`
	Version       string              `json:"version"`
	Build         string              `json:"build"`
	Commit        string              `json:"commit"`
	PublishedAt   string              `json:"published_at"`
	Downloads     map[string]Download `json:"downloads"`
}

type Result struct {
	Current         string `json:"current"`
	Latest          string `json:"latest,omitempty"`
	Previous        string `json:"previous,omitempty"`
	Updated         bool   `json:"updated"`
	UpdateAvailable bool   `json:"update_available"`
	RolledBack      bool   `json:"rolled_back,omitempty"`
	Path            string `json:"path,omitempty"`
}

type Config struct {
	ReleaseURL   string
	HTTPClient   *http.Client
	Current      buildinfo.Info
	Executable   string
	ExpectedPath string
	PreviousPath string
	CheckOnly    bool
	Rollback     bool
	AllowHTTP    bool // Test-only escape hatch; production callers leave this false.
	RunVersion   func(context.Context, string) (buildinfo.Info, error)
}

func Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.CheckOnly && cfg.Rollback {
		return Result{}, errors.New("update check and rollback cannot be requested together")
	}
	if cfg.ReleaseURL == "" {
		cfg.ReleaseURL = defaultReleaseURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	clientCopy := *cfg.HTTPClient
	if !cfg.AllowHTTP {
		previousRedirectCheck := clientCopy.CheckRedirect
		clientCopy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" {
				return errors.New("update redirect must use HTTPS")
			}
			if previousRedirectCheck != nil {
				return previousRedirectCheck(request, via)
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		}
	}
	cfg.HTTPClient = &clientCopy
	if cfg.Current.Version == "" {
		cfg.Current = buildinfo.Current()
	}
	if cfg.Executable == "" {
		path, err := os.Executable()
		if err != nil {
			return Result{}, fmt.Errorf("find current executable: %w", err)
		}
		cfg.Executable = path
	}
	if cfg.ExpectedPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Result{}, fmt.Errorf("find home directory: %w", err)
		}
		cfg.ExpectedPath = filepath.Join(home, ".local", "bin", "kit")
	}
	if cfg.RunVersion == nil {
		cfg.RunVersion = runVersion
	}
	if cfg.Rollback {
		return rollback(ctx, cfg)
	}

	releaseURL, err := url.Parse(cfg.ReleaseURL)
	if err != nil || releaseURL.Host == "" {
		return Result{}, errors.New("invalid release metadata URL")
	}
	if releaseURL.Scheme != "https" && !cfg.AllowHTTP {
		return Result{}, errors.New("release metadata URL must use HTTPS")
	}
	release, err := fetchRelease(ctx, cfg.HTTPClient, releaseURL, cfg.AllowHTTP)
	if err != nil {
		return Result{}, err
	}
	if err := validateRelease(release); err != nil {
		return Result{}, err
	}

	cmp, err := CompareSemver(cfg.Current.Version, release.Version)
	if err != nil {
		return Result{}, fmt.Errorf("current version %q cannot self-update: %w", cfg.Current.Version, err)
	}
	result := Result{
		Current:         cfg.Current.Version,
		Latest:          release.Version,
		UpdateAvailable: cmp < 0,
	}
	if cfg.CheckOnly || cmp >= 0 {
		return result, nil
	}

	key, err := targetKey(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return Result{}, err
	}
	asset, ok := release.Downloads[key]
	if !ok {
		return Result{}, fmt.Errorf("release %s does not contain an asset for %s", release.Version, key)
	}
	assetURL, err := releaseURL.Parse(asset.URL)
	if err != nil || assetURL.Host == "" {
		return Result{}, errors.New("invalid release download URL")
	}
	if assetURL.Scheme != "https" && !cfg.AllowHTTP {
		return Result{}, errors.New("release download URL must use HTTPS")
	}
	if !sameOrigin(releaseURL, assetURL) {
		return Result{}, errors.New("release download URL must use the release metadata origin")
	}
	if !strings.EqualFold(assetURL.Host, releaseURL.Host) {
		return Result{}, errors.New("release download URL must use the release metadata host")
	}

	executable, expected, err := installedExecutable(cfg)
	if err != nil {
		return Result{}, err
	}
	previousPath, err := previousBinaryPath(cfg, expected)
	if err != nil {
		return Result{}, err
	}

	temp, err := os.CreateTemp(filepath.Dir(executable), ".kit-update-*")
	if err != nil {
		return Result{}, fmt.Errorf("create update file next to %s: %w", executable, err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	digest, copyErr := download(ctx, cfg.HTTPClient, assetURL.String(), temp, cfg.AllowHTTP)
	closeErr := temp.Close()
	if copyErr != nil {
		return Result{}, copyErr
	}
	if closeErr != nil {
		return Result{}, fmt.Errorf("close downloaded binary: %w", closeErr)
	}
	if !strings.EqualFold(digest, strings.TrimSpace(asset.SHA256)) {
		return Result{}, fmt.Errorf("checksum mismatch: expected %s, got %s", asset.SHA256, digest)
	}
	if err := os.Chmod(tempPath, 0o755); err != nil {
		return Result{}, fmt.Errorf("make downloaded binary executable: %w", err)
	}
	newInfo, err := cfg.RunVersion(ctx, tempPath)
	if err != nil {
		return Result{}, fmt.Errorf("verify downloaded binary: %w", err)
	}
	expectedTarget := runtime.GOOS + "/" + runtime.GOARCH
	if newInfo.Version != release.Version || newInfo.Commit != release.Commit || newInfo.BuildDate != release.PublishedAt || newInfo.Target != expectedTarget {
		return Result{}, fmt.Errorf("downloaded binary metadata does not match release.json")
	}
	if err := preserveCurrentBinary(executable, previousPath); err != nil {
		return Result{}, fmt.Errorf("preserve current binary for rollback: %w", err)
	}
	if err := os.Rename(tempPath, executable); err != nil {
		return Result{}, fmt.Errorf("replace %s: %w", executable, err)
	}
	result.Updated = true
	result.Path = executable
	result.Previous = cfg.Current.Version
	return result, nil
}

func fetchRelease(ctx context.Context, client *http.Client, releaseURL *url.URL, allowHTTP bool) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL.String(), nil)
	if err != nil {
		return Release{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("download release metadata: %w", err)
	}
	defer resp.Body.Close()
	if err := requireSecureResponse(resp, releaseURL, allowHTTP); err != nil {
		return Release{}, fmt.Errorf("download release metadata: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("download release metadata: HTTP %d", resp.StatusCode)
	}
	var release Release
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&release); err != nil {
		return Release{}, fmt.Errorf("decode release metadata: %w", err)
	}
	return release, nil
}

func validateRelease(release Release) error {
	if release.SchemaVersion != 1 {
		return fmt.Errorf("unsupported release metadata schema %d", release.SchemaVersion)
	}
	if _, err := parseSemver(release.Version); err != nil {
		return fmt.Errorf("invalid release version: %w", err)
	}
	if len(release.Commit) != 40 {
		return errors.New("release metadata has an invalid commit")
	}
	if _, err := hex.DecodeString(release.Commit); err != nil {
		return errors.New("release metadata has an invalid commit")
	}
	if len(release.Build) < 7 || len(release.Build) > len(release.Commit) {
		return errors.New("release metadata has an invalid build commit")
	}
	if _, err := hex.DecodeString(release.Build); err != nil || !strings.HasPrefix(strings.ToLower(release.Commit), strings.ToLower(release.Build)) {
		return errors.New("release metadata build does not match commit")
	}
	if _, err := time.Parse(time.RFC3339, release.PublishedAt); err != nil {
		return errors.New("release metadata has an invalid published_at")
	}
	if len(release.Downloads) == 0 {
		return errors.New("release metadata has no downloads")
	}
	for name, asset := range release.Downloads {
		if asset.URL == "" {
			return fmt.Errorf("release asset %s has no URL", name)
		}
		if len(asset.SHA256) != 64 {
			return fmt.Errorf("release asset %s has an invalid SHA-256", name)
		}
		if _, err := hex.DecodeString(asset.SHA256); err != nil {
			return fmt.Errorf("release asset %s has an invalid SHA-256", name)
		}
	}
	return nil
}

func download(ctx context.Context, client *http.Client, downloadURL string, target io.Writer, allowHTTP bool) (string, error) {
	expectedURL, err := url.Parse(downloadURL)
	if err != nil {
		return "", fmt.Errorf("parse update URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download update: %w", err)
	}
	defer resp.Body.Close()
	if err := requireSecureResponse(resp, expectedURL, allowHTTP); err != nil {
		return "", fmt.Errorf("download update: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download update: HTTP %d", resp.StatusCode)
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: resp.Body, N: 100<<20 + 1}
	written, err := io.Copy(io.MultiWriter(target, hash), limited)
	if err != nil {
		return "", fmt.Errorf("save update: %w", err)
	}
	if written > 100<<20 {
		return "", errors.New("downloaded binary exceeds 100 MiB")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func requireSecureResponse(resp *http.Response, expected *url.URL, allowHTTP bool) error {
	if resp.Request == nil || resp.Request.URL == nil {
		return errors.New("response URL is unavailable")
	}
	if !allowHTTP && resp.Request.URL.Scheme != "https" {
		return errors.New("redirected response must use HTTPS")
	}
	if !sameOrigin(expected, resp.Request.URL) {
		return errors.New("redirected response must stay on the original origin")
	}
	return nil
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectivePort(left) == effectivePort(right)
}

func effectivePort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	if strings.EqualFold(value.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(value.Scheme, "http") {
		return "80"
	}
	return ""
}

func runVersion(ctx context.Context, binary string) (buildinfo.Info, error) {
	cmd := exec.CommandContext(ctx, binary, "version", "--json")
	out, err := cmd.Output()
	if err != nil {
		return buildinfo.Info{}, err
	}
	var info buildinfo.Info
	if err := json.Unmarshal(out, &info); err != nil {
		return buildinfo.Info{}, err
	}
	return info, nil
}

func targetKey(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "darwin/arm64":
		return "darwin-arm64", nil
	case "linux/amd64":
		return "linux-amd64", nil
	default:
		return "", fmt.Errorf("unsupported platform %s/%s; supported: darwin/arm64, linux/amd64", goos, goarch)
	}
}

type semver [3]int

func parseSemver(value string) (semver, error) {
	if !strings.HasPrefix(value, "v") {
		return semver{}, errors.New("version must start with v")
	}
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 3 {
		return semver{}, errors.New("version must be vMAJOR.MINOR.PATCH")
	}
	var result semver
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semver{}, errors.New("version has an invalid numeric component")
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return semver{}, errors.New("version has an invalid numeric component")
		}
		result[i] = n
	}
	return result, nil
}

// CompareSemver returns -1 when current is older, 0 when equal, and 1 when newer.
func CompareSemver(current, latest string) (int, error) {
	a, err := parseSemver(current)
	if err != nil {
		return 0, err
	}
	b, err := parseSemver(latest)
	if err != nil {
		return 0, err
	}
	for i := range a {
		if a[i] < b[i] {
			return -1, nil
		}
		if a[i] > b[i] {
			return 1, nil
		}
	}
	return 0, nil
}

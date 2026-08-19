package review

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"kit/internal/hosting"
)

type Status string

const (
	StatusOpen   Status = "open"
	StatusMerged Status = "merged"
	StatusClosed Status = "closed"
)

var ErrUnsupportedProvider = errors.New("review provider is not supported")

type CreateRequest struct {
	SourceBranch       string `json:"source_branch"`
	TargetBranch       string `json:"target_branch"`
	Title              string `json:"title"`
	Description        string `json:"description,omitempty"`
	Draft              bool   `json:"draft,omitempty"`
	RemoveSourceBranch bool   `json:"remove_source_branch,omitempty"`
}

type Review struct {
	Provider     string     `json:"provider"`
	ID           string     `json:"id"`
	Number       int64      `json:"number"`
	URL          string     `json:"url"`
	Status       Status     `json:"status"`
	SourceBranch string     `json:"source_branch"`
	TargetBranch string     `json:"target_branch"`
	Title        string     `json:"title"`
	SourceSHA    string     `json:"source_sha,omitempty"`
	MergeSHA     string     `json:"merge_sha,omitempty"`
	MergedAt     *time.Time `json:"merged_at,omitempty"`
}

type Client interface {
	FindOpen(ctx context.Context, sourceBranch, targetBranch string) ([]Review, error)
	Find(ctx context.Context, sourceBranch, targetBranch string) ([]Review, error)
	Create(ctx context.Context, request CreateRequest) (Review, error)
	Get(ctx context.Context, id string) (Review, error)
}

type Options struct {
	// HTTPClient is primarily intended for tests with an httptest TLS server.
	// Timeout and redirect restrictions are applied to a copy of this client.
	HTTPClient *http.Client
	// APIBaseURL is primarily intended for tests. Its HTTPS origin must exactly
	// match Repository.Host, so it cannot bypass the production origin checks.
	APIBaseURL string
	// Getenv allows tests to supply credentials without mutating process state.
	Getenv func(string) string
}

func NewClient(repository hosting.Repository) (Client, error) {
	return NewClientWithOptions(repository, Options{})
}

func NewClientWithOptions(repository hosting.Repository, options Options) (Client, error) {
	provider := strings.ToLower(repository.Provider)
	if provider != "gitlab" && provider != "forgejo" {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedProvider, repository.Provider)
	}
	if err := validateRepository(repository, provider); err != nil {
		return nil, err
	}

	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	tokenName := "KIT_GITLAB_TOKEN"
	hostName := "KIT_GITLAB_HOST"
	if provider == "forgejo" {
		tokenName = "KIT_FORGEJO_TOKEN"
		hostName = "KIT_FORGEJO_HOST"
	}
	token := getenv(tokenName)
	configuredHost := getenv(hostName)
	if token == "" {
		return nil, fmt.Errorf("%s is required", tokenName)
	}
	// gitlab.com is the only provider with a safe public default. Self-hosted
	// GitLab and every Forgejo server must explicitly bind the token to a host.
	if configuredHost == "" && provider == "gitlab" && repository.Host == "gitlab.com" {
		configuredHost = "gitlab.com"
	}
	if err := validateConfiguredHost(configuredHost); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", hostName, err)
	}
	if configuredHost != repository.Host {
		return nil, fmt.Errorf("%s does not match repository host", hostName)
	}

	baseURL := "https://" + repository.Host
	if options.APIBaseURL != "" {
		baseURL = options.APIBaseURL
	}
	origin, err := validateAPIBase(baseURL, repository.Host)
	if err != nil {
		return nil, err
	}
	client := secureHTTPClient(options.HTTPClient, origin)
	base := apiClient{baseURL: strings.TrimSuffix(baseURL, "/"), token: token, httpClient: client}
	if provider == "gitlab" {
		return &gitLabClient{apiClient: base, projectPath: repository.Path}, nil
	}
	return &forgejoClient{apiClient: base, owner: repository.Owner, repository: repository.Name}, nil
}

func validateRepository(repository hosting.Repository, provider string) error {
	if err := validateConfiguredHost(repository.Host); err != nil {
		return fmt.Errorf("invalid repository host: %w", err)
	}
	if repository.Path == "" || repository.Owner == "" || repository.Name == "" {
		return errors.New("repository path, owner, and name are required")
	}
	if path.Clean(repository.Path) != repository.Path || strings.Contains(repository.Path, "\\") || repository.Owner+"/"+repository.Name != repository.Path {
		return errors.New("repository coordinates are inconsistent or unsafe")
	}
	if provider == "forgejo" && strings.Contains(repository.Owner, "/") {
		return errors.New("Forgejo repository owner must contain one path segment")
	}
	return nil
}

func validateReview(item Review) error {
	if item.Provider != "gitlab" && item.Provider != "forgejo" {
		return errors.New("review API response contains an invalid provider")
	}
	if item.Number <= 0 || item.ID != strconv.FormatInt(item.Number, 10) {
		return errors.New("review API response contains an invalid review number")
	}
	if item.SourceBranch == "" || item.TargetBranch == "" {
		return errors.New("review API response is missing branch information")
	}
	if item.Status != StatusOpen && item.Status != StatusMerged && item.Status != StatusClosed {
		return errors.New("review API response contains an invalid status")
	}
	parsed, err := url.Parse(item.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("review API response contains an invalid review URL")
	}
	return nil
}

func validateConfiguredHost(host string) error {
	if host == "" {
		return errors.New("host is required")
	}
	if host != strings.ToLower(host) || strings.ContainsAny(host, "/?#@") {
		return errors.New("host must be exact lowercase host[:port] without a scheme or path")
	}
	parsed, err := url.Parse("https://" + host)
	if err != nil || parsed.Host != host || parsed.Hostname() == "" {
		return errors.New("host must be a valid host[:port]")
	}
	return nil
}

func validateAPIBase(baseURL, repositoryHost string) (*url.URL, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, errors.New("invalid review API base URL")
	}
	if parsed.Scheme != "https" || parsed.Host != repositoryHost || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("review API base URL must be the repository HTTPS origin")
	}
	return parsed, nil
}

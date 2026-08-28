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
	Create(ctx context.Context, request CreateRequest) (Review, error)
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
	// Lookup resolves a stored credential after the provider-specific
	// environment override has been considered. A nil lookup preserves the
	// legacy environment-only behavior.
	Lookup func(provider, host string) (string, error)
}

func NewClient(repository hosting.Repository) (Client, error) {
	return NewClientWithOptions(repository, Options{})
}

func NewClientWithOptions(repository hosting.Repository, options Options) (Client, error) {
	provider := strings.ToLower(repository.Provider)
	if provider != "gitea" && provider != "gitlab" && provider != "forgejo" {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedProvider, repository.Provider)
	}
	if err := validateRepository(repository, provider); err != nil {
		return nil, err
	}
	originScheme, err := reviewOriginScheme(repository, provider)
	if err != nil {
		return nil, err
	}

	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	tokenName := "KIT_GITLAB_TOKEN"
	hostName := "KIT_GITLAB_HOST"
	if provider == "gitea" {
		tokenName = "KIT_GITEA_TOKEN"
		hostName = "KIT_GITEA_HOST"
	} else if provider == "forgejo" {
		tokenName = "KIT_FORGEJO_TOKEN"
		hostName = "KIT_FORGEJO_HOST"
	}
	token := getenv(tokenName)
	configuredHost := getenv(hostName)
	if provider == "gitea" && (token == "") != (configuredHost == "") {
		return nil, fmt.Errorf("%s and %s must be set together", tokenName, hostName)
	}
	if token == "" && configuredHost == "" && provider == "gitea" && options.Lookup != nil {
		var err error
		token, err = options.Lookup(provider, repository.Host)
		if err != nil {
			return nil, fmt.Errorf("Gitea credential lookup failed: %w; run %s", err, loginCommand(repository.Host))
		}
		configuredHost = repository.Host
	}
	if token == "" {
		if provider == "gitea" {
			return nil, fmt.Errorf("Gitea credential is required; run %s", loginCommand(repository.Host))
		}
		return nil, fmt.Errorf("%s is required", tokenName)
	}
	// gitlab.com is the only legacy provider with a safe public default.
	// Self-hosted GitLab and every Gitea/Forgejo server must explicitly bind
	// the token to an exact lowercase host.
	if configuredHost == "" && provider == "gitlab" && repository.Host == "gitlab.com" {
		configuredHost = "gitlab.com"
	}
	if err := validateConfiguredHost(configuredHost); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", hostName, err)
	}
	if configuredHost != repository.Host {
		return nil, fmt.Errorf("%s does not match repository host", hostName)
	}

	baseURL := originScheme + "://" + repository.Host
	if options.APIBaseURL != "" {
		baseURL = options.APIBaseURL
	}
	origin, err := validateAPIBase(baseURL, originScheme, repository.Host)
	if err != nil {
		return nil, err
	}
	client := secureHTTPClient(options.HTTPClient, origin)
	base := apiClient{baseURL: strings.TrimSuffix(baseURL, "/"), token: token, httpClient: client}
	if provider == "gitlab" {
		return &gitLabClient{apiClient: base, projectPath: repository.Path}, nil
	}
	if provider == "gitea" {
		return &giteaClient{apiClient: base, owner: repository.Owner, repository: repository.Name}, nil
	}
	return &forgejoClient{apiClient: base, owner: repository.Owner, repository: repository.Name}, nil
}

func reviewOriginScheme(repository hosting.Repository, provider string) (string, error) {
	if repository.Scheme != "http" {
		return "https", nil
	}
	if provider != "gitea" {
		return "", errors.New("insecure HTTP review access is supported only for Gitea")
	}
	if !repository.AllowInsecureHTTP {
		return "", errors.New("HTTP Gitea remote requires repository config git.allow-insecure-http=true")
	}
	if !hosting.IsPrivateLiteralHost(repository.Host) {
		return "", errors.New("HTTP Gitea remote host must be a literal private, loopback, or link-local IP address")
	}
	return "http", nil
}

func loginCommand(host string) string {
	// Hosts accepted by validateConfiguredHost contain no shell metacharacters
	// beyond the safe host[:port] alphabet.
	return "kit auth login gitea --host " + host
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
	if (provider == "gitea" || provider == "forgejo") && strings.Contains(repository.Owner, "/") {
		return fmt.Errorf("%s repository owner must contain one path segment", provider)
	}
	return nil
}

func validateReview(item Review) error {
	if item.Provider != "gitea" && item.Provider != "gitlab" && item.Provider != "forgejo" {
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
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
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

func validateAPIBase(baseURL, repositoryScheme, repositoryHost string) (*url.URL, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, errors.New("invalid review API base URL")
	}
	if parsed.Scheme != repositoryScheme || parsed.Host != repositoryHost || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("review API base URL must exactly match the allowed repository origin")
	}
	return parsed, nil
}

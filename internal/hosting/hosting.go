package hosting

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
)

type Repository struct {
	Provider string `json:"provider"`
	Remote   string `json:"remote_url"`
	WebURL   string `json:"web_url,omitempty"`
	Host     string `json:"host,omitempty"`
	Path     string `json:"path,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Name     string `json:"name,omitempty"`
}

func Resolve(configuredProvider, remote string) Repository {
	host, repositoryPath, scheme := parseRemote(remote)
	provider := strings.ToLower(configuredProvider)
	if provider == "" || provider == "auto" {
		switch {
		case strings.Contains(strings.ToLower(host), "gitlab"):
			provider = "gitlab"
		case strings.Contains(strings.ToLower(host), "forgejo"), strings.Contains(strings.ToLower(host), "gitea"):
			provider = "forgejo"
		default:
			provider = "generic"
		}
	}
	webScheme := scheme
	if webScheme == "" || webScheme == "ssh" {
		webScheme = "https"
	}
	webURL := ""
	if host != "" && repositoryPath != "" {
		webURL = webScheme + "://" + host + "/" + repositoryPath
	}
	owner, name := splitRepositoryPath(repositoryPath)
	return Repository{
		Provider: provider,
		Remote:   sanitizeRemote(remote),
		WebURL:   webURL,
		Host:     host,
		Path:     repositoryPath,
		Owner:    owner,
		Name:     name,
	}
}

func sanitizeRemote(remote string) string {
	parsed, err := url.Parse(remote)
	if err == nil && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	}
	if strings.Contains(remote, "://") {
		return ""
	}
	if colon := strings.Index(remote, ":"); colon > 0 && !strings.Contains(remote[:colon], "/") {
		left := remote[:colon]
		if at := strings.LastIndex(left, "@"); at >= 0 {
			left = left[at+1:]
		}
		return left + remote[colon:]
	}
	return remote
}

func (r Repository) ReviewName() string {
	switch r.Provider {
	case "gitlab":
		return "Merge Request"
	case "forgejo":
		return "Pull Request"
	default:
		return "review"
	}
}

func (r Repository) ReviewURL(source, target string) string {
	if r.WebURL == "" {
		return ""
	}
	switch r.Provider {
	case "gitlab":
		query := url.Values{}
		query.Set("merge_request[source_branch]", source)
		query.Set("merge_request[target_branch]", target)
		return r.WebURL + "/-/merge_requests/new?" + query.Encode()
	case "forgejo":
		return fmt.Sprintf("%s/compare/%s...%s", r.WebURL, url.PathEscape(target), url.PathEscape(source))
	default:
		return r.WebURL
	}
}

func parseRemote(remote string) (host, repositoryPath, scheme string) {
	if parsed, err := url.Parse(remote); err == nil && parsed.Host != "" {
		host = parsed.Hostname()
		if parsed.Port() != "" {
			host = net.JoinHostPort(host, parsed.Port())
		} else if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		return strings.ToLower(host), cleanPath(parsed.Path), strings.ToLower(parsed.Scheme)
	}
	if strings.Contains(remote, "://") {
		return "", "", ""
	}
	// Git's SCP-like syntax: git@example.com:owner/repository.git
	if colon := strings.Index(remote, ":"); colon > 0 && !strings.Contains(remote[:colon], "/") {
		left, right := remote[:colon], remote[colon+1:]
		if at := strings.LastIndex(left, "@"); at >= 0 {
			left = left[at+1:]
		}
		return strings.ToLower(left), cleanPath(right), "https"
	}
	return "", "", ""
}

func cleanPath(value string) string {
	value = strings.TrimSuffix(strings.Trim(value, "/"), ".git")
	if value == "." || value == "" || strings.Contains(value, "\\") {
		return ""
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return ""
		}
	}
	return cleaned
}

func splitRepositoryPath(repositoryPath string) (owner, name string) {
	index := strings.LastIndex(repositoryPath, "/")
	if index <= 0 || index == len(repositoryPath)-1 {
		return "", ""
	}
	return repositoryPath[:index], repositoryPath[index+1:]
}

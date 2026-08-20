package hosting

import (
	"strings"
	"testing"
)

func TestResolveGitLabSCPRemote(t *testing.T) {
	repository := Resolve("auto", "git@gitlab.example.com:group/project.git")
	if repository.Provider != "gitlab" || repository.WebURL != "https://gitlab.example.com/group/project" {
		t.Fatalf("unexpected repository: %#v", repository)
	}
	url := repository.ReviewURL("feat/login", "develop")
	if !strings.Contains(url, "/-/merge_requests/new?") || !strings.Contains(url, "source_branch") || !strings.Contains(url, "target_branch") {
		t.Fatalf("unexpected GitLab review URL: %s", url)
	}
	if repository.Host != "gitlab.example.com" || repository.Path != "group/project" || repository.Owner != "group" || repository.Name != "project" {
		t.Fatalf("unexpected repository coordinates: %#v", repository)
	}
}

func TestExplicitForgejoProviderOnPrivateHost(t *testing.T) {
	repository := Resolve("forgejo", "ssh://git@git.2juho.com/juho/kit.git")
	if repository.Provider != "forgejo" || repository.WebURL != "https://git.2juho.com/juho/kit" {
		t.Fatalf("unexpected repository: %#v", repository)
	}
	if got := repository.ReviewURL("feat/login", "develop"); got != "https://git.2juho.com/juho/kit/compare/develop...feat%2Flogin" {
		t.Fatalf("unexpected Forgejo review URL: %s", got)
	}
}

func TestResolveGiteaAndRequireExplicitProviderForUnidentifiedPrivateHost(t *testing.T) {
	auto := Resolve("auto", "git@gitea.company.example:team/project.git")
	if auto.Provider != "gitea" || auto.ReviewName() != "Pull Request" {
		t.Fatalf("unexpected Gitea repository: %#v", auto)
	}
	if got := auto.ReviewURL("feat/login", "develop"); got != "https://gitea.company.example/team/project/compare/develop...feat%2Flogin" {
		t.Fatalf("unexpected Gitea review URL: %s", got)
	}

	private := Resolve("auto", "git@git.company.example:team/project.git")
	if private.Provider != "generic" {
		t.Fatalf("unidentified private host must stay generic: %#v", private)
	}
	explicit := Resolve("gitea", "git@git.company.example:team/project.git")
	if explicit.Provider != "gitea" {
		t.Fatalf("explicit Gitea provider was not preserved: %#v", explicit)
	}
}

func TestResolveNestedGitLabOwnerAndLowercaseHost(t *testing.T) {
	repository := Resolve("gitlab", "ssh://git@GITLAB.EXAMPLE.COM/group/subgroup/project.git")
	if repository.Host != "gitlab.example.com" || repository.Owner != "group/subgroup" || repository.Name != "project" {
		t.Fatalf("unexpected repository coordinates: %#v", repository)
	}
}

func TestResolveRejectsUnsafeRepositoryPath(t *testing.T) {
	repository := Resolve("gitlab", "https://gitlab.example.com/group/../project.git")
	if repository.Path != "" || repository.Owner != "" || repository.Name != "" || repository.WebURL != "" {
		t.Fatalf("unsafe repository path accepted: %#v", repository)
	}
}

func TestResolveHTTPSIPv6Host(t *testing.T) {
	repository := Resolve("gitlab", "https://[2001:db8::1]:8443/group/project.git")
	if repository.Host != "[2001:db8::1]:8443" || repository.WebURL != "https://[2001:db8::1]:8443/group/project" {
		t.Fatalf("unexpected IPv6 repository: %#v", repository)
	}
}

func TestResolveDoesNotExposeRemoteCredentials(t *testing.T) {
	repository := Resolve("gitlab", "https://user:secret@gitlab.example.com/group/project.git?access_token=query-secret#fragment-secret")
	if strings.Contains(repository.Remote, "user") || strings.Contains(repository.Remote, "secret") || strings.ContainsAny(repository.Remote, "?#") {
		t.Fatalf("credentials leaked in remote: %s", repository.Remote)
	}
	if strings.Contains(repository.WebURL, "user") || strings.Contains(repository.WebURL, "secret") {
		t.Fatalf("credentials leaked in web URL: %s", repository.WebURL)
	}
}

func TestResolveDoesNotExposeCredentialsFromMalformedURL(t *testing.T) {
	repository := Resolve("gitlab", "https://user:secret@")
	if strings.Contains(repository.Remote, "user") || strings.Contains(repository.Remote, "secret") || repository.WebURL != "" {
		t.Fatalf("credentials leaked from malformed remote: %#v", repository)
	}
}

func TestInsecureHTTPRequiresExplicitGiteaPrivateLiteralRemote(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		remote   string
		optIn    bool
		allowed  bool
	}{
		{name: "RFC1918", provider: "gitea", remote: "http://10.20.30.40:3000/owner/repo.git", optIn: true, allowed: true},
		{name: "IPv4 loopback", provider: "gitea", remote: "http://127.0.0.1:3000/owner/repo.git", optIn: true, allowed: true},
		{name: "IPv4 link local", provider: "gitea", remote: "http://169.254.1.2/owner/repo.git", optIn: true, allowed: true},
		{name: "IPv6 ULA", provider: "gitea", remote: "http://[fd00::1]:3000/owner/repo.git", optIn: true, allowed: true},
		{name: "IPv6 loopback", provider: "gitea", remote: "http://[::1]:3000/owner/repo.git", optIn: true, allowed: true},
		{name: "IPv6 link local", provider: "gitea", remote: "http://[fe80::1]/owner/repo.git", optIn: true, allowed: true},
		{name: "missing opt in", provider: "gitea", remote: "http://10.0.0.2/owner/repo.git", allowed: false},
		{name: "hostname", provider: "gitea", remote: "http://gitea.lan/owner/repo.git", optIn: true, allowed: false},
		{name: "public IP", provider: "gitea", remote: "http://8.8.8.8/owner/repo.git", optIn: true, allowed: false},
		{name: "legacy provider", provider: "forgejo", remote: "http://10.0.0.2/owner/repo.git", optIn: true, allowed: false},
		{name: "SSH remote", provider: "gitea", remote: "ssh://git@10.0.0.2/owner/repo.git", optIn: true, allowed: false},
		{name: "SCP remote", provider: "gitea", remote: "git@10.0.0.2:owner/repo.git", optIn: true, allowed: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := Resolve(test.provider, test.remote)
			repository.AllowInsecureHTTP = test.optIn
			if got := repository.InsecureHTTPAllowed(); got != test.allowed {
				t.Fatalf("allowed=%t, want %t: %#v", got, test.allowed, repository)
			}
		})
	}
}

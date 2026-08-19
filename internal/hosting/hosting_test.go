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

package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kit/internal/hosting"
)

func TestNewClientRejectsGenericProviderAndHostMismatch(t *testing.T) {
	_, err := NewClientWithOptions(hosting.Resolve("generic", "git@example.com:owner/repo.git"), Options{})
	if !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("expected unsupported provider, got %v", err)
	}

	repository := hosting.Resolve("gitea", "git@git.example.com:owner/repo.git")
	_, err = NewClientWithOptions(repository, Options{Getenv: environment(map[string]string{
		"KIT_GITEA_TOKEN": "secret-token",
		"KIT_GITEA_HOST":  "other.example.com",
	})})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("expected safe host mismatch error, got %v", err)
	}
}

func TestNewClientRequiresExactLowercaseConfiguredHost(t *testing.T) {
	repository := hosting.Resolve("gitea", "git@git.example.com:owner/repo.git")
	_, err := NewClientWithOptions(repository, Options{Getenv: environment(map[string]string{
		"KIT_GITEA_TOKEN": "secret-token",
		"KIT_GITEA_HOST":  "Git.Example.com",
	})})
	if err == nil || !strings.Contains(err.Error(), "exact lowercase") {
		t.Fatalf("expected lowercase host error, got %v", err)
	}
}

func TestGiteaRequiresExplicitHostBinding(t *testing.T) {
	repository := hosting.Resolve("gitea", "git@gitea.example.com:owner/repo.git")
	_, err := NewClientWithOptions(repository, Options{Getenv: environment(map[string]string{
		"KIT_GITEA_TOKEN": "secret-token",
	})})
	if err == nil || !strings.Contains(err.Error(), "KIT_GITEA_HOST") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("expected safe explicit host error, got %v", err)
	}
}

func TestGiteaStoredCredentialLookupAndEnvironmentPrecedence(t *testing.T) {
	repository := hosting.Resolve("gitea", "git@git.example.com:owner/repo.git")
	lookupCalls := 0
	client, err := NewClientWithOptions(repository, Options{
		Getenv: environment(nil),
		Lookup: func(provider, host string) (string, error) {
			lookupCalls++
			if provider != "gitea" || host != "git.example.com" {
				t.Fatalf("unexpected lookup: %s %s", provider, host)
			}
			return "stored-secret", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if lookupCalls != 1 || client.(*giteaClient).token != "stored-secret" {
		t.Fatalf("stored credential was not used: calls=%d, client=%#v", lookupCalls, client)
	}

	lookupCalls = 0
	client, err = NewClientWithOptions(repository, Options{
		Getenv: environment(map[string]string{
			"KIT_GITEA_TOKEN": "environment-secret",
			"KIT_GITEA_HOST":  "git.example.com",
		}),
		Lookup: func(string, string) (string, error) {
			lookupCalls++
			return "stored-secret", nil
		},
	})
	if err != nil || lookupCalls != 0 || client.(*giteaClient).token != "environment-secret" {
		t.Fatalf("environment override did not win: calls=%d, client=%#v, err=%v", lookupCalls, client, err)
	}
}

func TestGiteaEnvironmentOverrideRequiresTokenAndHostPair(t *testing.T) {
	repository := hosting.Resolve("gitea", "git@git.example.com:owner/repo.git")
	for _, values := range []map[string]string{
		{"KIT_GITEA_TOKEN": "secret"},
		{"KIT_GITEA_HOST": "git.example.com"},
	} {
		_, err := NewClientWithOptions(repository, Options{Getenv: environment(values)})
		if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "must be set together") {
			t.Fatalf("unsafe partial override result: %v", err)
		}
	}
}

func TestGiteaMissingStoredCredentialProvidesLoginGuidance(t *testing.T) {
	repository := hosting.Resolve("gitea", "git@git.example.com:owner/repo.git")
	_, err := NewClientWithOptions(repository, Options{
		Getenv: environment(nil),
		Lookup: func(string, string) (string, error) { return "", errors.New("not found") },
	})
	if err == nil || !strings.Contains(err.Error(), "kit auth login gitea --host git.example.com") {
		t.Fatalf("missing safe login guidance: %v", err)
	}
}

func TestNewClientRejectsInconsistentRepositoryCoordinates(t *testing.T) {
	repository := hosting.Repository{
		Provider: "gitea",
		Host:     "git.example.com",
		Path:     "owner/../other/repo",
		Owner:    "owner/../other",
		Name:     "repo",
	}
	_, err := NewClientWithOptions(repository, Options{Getenv: environment(map[string]string{
		"KIT_GITEA_TOKEN": "secret",
		"KIT_GITEA_HOST":  "git.example.com",
	})})
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("expected unsafe coordinates error, got %v", err)
	}
}

func TestGitLabDotComAllowsPublicHostDefault(t *testing.T) {
	repository := hosting.Resolve("gitlab", "git@gitlab.com:owner/repo.git")
	client, err := NewClientWithOptions(repository, Options{Getenv: environment(map[string]string{
		"KIT_GITLAB_TOKEN": "secret",
	})})
	if err != nil || client == nil {
		t.Fatalf("expected gitlab.com public host default, got %T, %v", client, err)
	}
}

func TestGitLabCreateFindAndGet(t *testing.T) {
	mergedAt := time.Now().UTC().Truncate(time.Second)
	var calls int
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.Header.Get("Private-Token") != "gitlab-secret" {
			t.Errorf("missing GitLab token header")
		}
		if !strings.HasPrefix(request.URL.EscapedPath(), "/api/v4/projects/group%2Fsubgroup%2Frepo/merge_requests") {
			t.Errorf("unexpected URI path: %s", request.URL.EscapedPath())
		}
		writer.Header().Set("Content-Type", "application/json")
		switch calls {
		case 1:
			if request.Method != http.MethodPost {
				t.Errorf("unexpected method: %s", request.Method)
			}
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["source_branch"] != "feat/login" || payload["target_branch"] != "develop" || payload["remove_source_branch"] != true {
				t.Errorf("unexpected payload: %#v", payload)
			}
			fmt.Fprintf(writer, `{"id":900,"iid":42,"web_url":"https://gitlab.test/mr/42","state":"opened","title":"Login","source_branch":"feat/login","target_branch":"develop","sha":"source-sha"}`)
		case 2:
			query := request.URL.Query()
			if query.Get("state") != "opened" || query.Get("source_branch") != "feat/login" || query.Get("target_branch") != "develop" {
				t.Errorf("unexpected query: %v", query)
			}
			fmt.Fprintf(writer, `[{"id":900,"iid":42,"web_url":"https://gitlab.test/mr/42","state":"opened","title":"Login","source_branch":"feat/login","target_branch":"develop","sha":"source-sha"}]`)
		case 3:
			if !strings.HasSuffix(request.URL.Path, "/42") {
				t.Errorf("unexpected get path: %s", request.URL.Path)
			}
			fmt.Fprintf(writer, `{"id":900,"iid":42,"web_url":"https://gitlab.test/mr/42","state":"merged","title":"Login","source_branch":"feat/login","target_branch":"develop","sha":"source-sha","squash_commit_sha":"merge-sha","merged_at":%q}`, mergedAt.Format(time.RFC3339))
		}
	})

	client := newTestClient(t, "gitlab", "gitlab.test", "group/subgroup/repo", "gitlab-secret", handlerClient(handler))
	created, err := client.Create(context.Background(), CreateRequest{SourceBranch: "feat/login", TargetBranch: "develop", Title: "Login", RemoveSourceBranch: true})
	if err != nil || created.Number != 42 || created.Status != StatusOpen || created.SourceSHA != "source-sha" {
		t.Fatalf("unexpected created review: %#v, %v", created, err)
	}
	found, err := client.FindOpen(context.Background(), "feat/login", "develop")
	if err != nil || len(found) != 1 || found[0].ID != "42" {
		t.Fatalf("unexpected found reviews: %#v, %v", found, err)
	}
	got, err := client.Get(context.Background(), "42")
	if err != nil || got.Status != StatusMerged || got.MergeSHA != "merge-sha" || got.MergedAt == nil || !got.MergedAt.Equal(mergedAt) {
		t.Fatalf("unexpected merged review: %#v, %v", got, err)
	}
}

func TestGiteaCreateFindAndGet(t *testing.T) {
	var calls int
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.Header.Get("Authorization") != "token gitea-secret" {
			t.Errorf("missing Gitea authorization")
		}
		if !strings.HasPrefix(request.URL.Path, "/api/v1/repos/owner/repo/pulls") {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		switch calls {
		case 1:
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["head"] != "feat/login" || payload["base"] != "develop" || payload["title"] != "WIP: Login" {
				t.Errorf("unexpected payload: %#v", payload)
			}
			if _, exists := payload["draft"]; exists {
				t.Errorf("Gitea payload must not contain draft: %#v", payload)
			}
			if _, exists := payload["remove_source_branch"]; exists {
				t.Errorf("Gitea payload must not claim remote branch cleanup: %#v", payload)
			}
			fmt.Fprint(writer, giteaJSON(false, "open", "WIP: Login"))
		case 2:
			if request.URL.Query().Get("state") != "open" || request.URL.Query().Get("head") != "" || request.URL.Query().Get("base") != "" {
				t.Errorf("unexpected query: %v", request.URL.Query())
			}
			fmt.Fprintf(writer, "[%s]", giteaJSON(false, "open", "WIP: Login"))
		case 3:
			fmt.Fprint(writer, giteaJSON(true, "closed", "WIP: Login"))
		case 4:
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["title"] != "WIP: Existing" {
				t.Errorf("duplicated WIP prefix: %#v", payload)
			}
			fmt.Fprint(writer, giteaJSON(false, "open", "WIP: Existing"))
		}
	})

	client := newTestClient(t, "gitea", "gitea.test", "owner/repo", "gitea-secret", handlerClient(handler))
	created, err := client.Create(context.Background(), CreateRequest{SourceBranch: "feat/login", TargetBranch: "develop", Title: "Login", Draft: true, RemoveSourceBranch: true})
	if err != nil || created.Number != 7 || created.Provider != "gitea" || created.Status != StatusOpen {
		t.Fatalf("unexpected created PR: %#v, %v", created, err)
	}
	found, err := client.FindOpen(context.Background(), "feat/login", "develop")
	if err != nil || len(found) != 1 || found[0].SourceSHA != "head-sha" {
		t.Fatalf("unexpected found PRs: %#v, %v", found, err)
	}
	got, err := client.Get(context.Background(), "7")
	if err != nil || got.Status != StatusMerged || got.SourceBranch != "feat/login" || got.TargetBranch != "develop" || got.MergeSHA != "merge-sha" || got.MergedAt == nil {
		t.Fatalf("unexpected merged PR: %#v, %v", got, err)
	}
	if _, err := client.Create(context.Background(), CreateRequest{SourceBranch: "feat/login", TargetBranch: "develop", Title: "WIP: Existing", Draft: true}); err != nil {
		t.Fatal(err)
	}
}

func TestForgejoCreateFindAndGet(t *testing.T) {
	var calls int
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.Header.Get("Authorization") != "token forgejo-secret" {
			t.Errorf("missing Forgejo authorization")
		}
		if !strings.HasPrefix(request.URL.Path, "/api/v1/repos/owner/repo/pulls") {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		switch calls {
		case 1:
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["head"] != "feat/login" || payload["base"] != "develop" || payload["draft"] != true {
				t.Errorf("unexpected payload: %#v", payload)
			}
			fmt.Fprint(writer, forgejoJSON(false, "open"))
		case 2:
			if request.URL.Query().Get("state") != "open" || request.URL.Query().Get("head") != "" || request.URL.Query().Get("base") != "" {
				t.Errorf("unexpected query: %v", request.URL.Query())
			}
			fmt.Fprintf(writer, "[%s]", forgejoJSON(false, "open"))
		case 3:
			fmt.Fprint(writer, forgejoJSON(true, "closed"))
		}
	})

	client := newTestClient(t, "forgejo", "forgejo.test", "owner/repo", "forgejo-secret", handlerClient(handler))
	created, err := client.Create(context.Background(), CreateRequest{SourceBranch: "feat/login", TargetBranch: "develop", Title: "Login", Draft: true})
	if err != nil || created.Number != 7 || created.Status != StatusOpen {
		t.Fatalf("unexpected created PR: %#v, %v", created, err)
	}
	found, err := client.FindOpen(context.Background(), "feat/login", "develop")
	if err != nil || len(found) != 1 || found[0].SourceSHA != "head-sha" {
		t.Fatalf("unexpected found PRs: %#v, %v", found, err)
	}
	got, err := client.Get(context.Background(), "7")
	if err != nil || got.Status != StatusMerged || got.MergeSHA != "merge-sha" {
		t.Fatalf("unexpected merged PR: %#v, %v", got, err)
	}
}

func TestFindCanRecoverMergedReview(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		host     string
		path     string
		body     string
	}{
		{
			name: "gitlab", provider: "gitlab", host: "gitlab.test", path: "owner/repo",
			body: `[{"iid":8,"web_url":"https://gitlab.test/mr/8","state":"merged","title":"Merged","source_branch":"feat/recover","target_branch":"develop","sha":"published","merge_commit_sha":"merged"}]`,
		},
		{
			name: "forgejo", provider: "forgejo", host: "forgejo.test", path: "owner/repo",
			body: `[{"number":8,"html_url":"https://forgejo.test/owner/repo/pulls/8","state":"closed","merged":true,"title":"Merged","head":{"ref":"feat/recover","sha":"published"},"base":{"ref":"develop"},"merge_commit_sha":"merged"}]`,
		},
		{
			name: "gitea", provider: "gitea", host: "gitea.test", path: "owner/repo",
			body: `[{"number":8,"html_url":"https://gitea.test/owner/repo/pulls/8","state":"closed","merged":true,"title":"Merged","head":{"ref":"feat/recover","sha":"published"},"base":{"ref":"develop"},"merge_commit_sha":"merged"}]`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Query().Get("state") != "all" {
					t.Errorf("unexpected recovery state query: %v", request.URL.Query())
				}
				fmt.Fprint(writer, test.body)
			})
			client := newTestClient(t, test.provider, test.host, test.path, "secret", handlerClient(handler))
			found, err := client.Find(context.Background(), "feat/recover", "develop")
			if err != nil || len(found) != 1 || found[0].Status != StatusMerged || found[0].SourceSHA != "published" {
				t.Fatalf("merged review recovery: %#v, %v", found, err)
			}
		})
	}
}

func TestClientRejectsCrossOriginAndDowngradeRedirects(t *testing.T) {
	tests := []struct {
		name     string
		location string
	}{
		{name: "cross host", location: "https://other.test/review"},
		{name: "downgrade", location: "http://gitlab.test/review"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				http.Redirect(writer, request, test.location, http.StatusFound)
			})
			client := newTestClient(t, "gitlab", "gitlab.test", "owner/repo", "secret", handlerClient(handler))
			_, err := client.FindOpen(context.Background(), "feat", "develop")
			if err == nil || !strings.Contains(err.Error(), "redirect changed HTTPS origin") {
				t.Fatalf("expected protected redirect error, got %v", err)
			}
		})
	}
}

func TestClientLimitsResponseAndDoesNotExposeErrorBody(t *testing.T) {
	t.Run("size", func(t *testing.T) {
		handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Length", fmt.Sprint(maxResponseBytes+1))
			writer.WriteHeader(http.StatusOK)
		})
		client := newTestClient(t, "gitlab", "gitlab.test", "owner/repo", "secret", handlerClient(handler))
		_, err := client.FindOpen(context.Background(), "feat", "develop")
		if err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("expected size limit, got %v", err)
		}
	})
	t.Run("raw body", func(t *testing.T) {
		handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.Error(writer, "server-secret-body", http.StatusUnauthorized)
		})
		client := newTestClient(t, "gitlab", "gitlab.test", "owner/repo", "secret-token", handlerClient(handler))
		_, err := client.FindOpen(context.Background(), "feat", "develop")
		if err == nil || strings.Contains(err.Error(), "server-secret-body") || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("unsafe API error: %v", err)
		}
	})
}

func TestClientRejectsInvalidProviderDTO(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, `[{"iid":0,"web_url":"http://unsafe.example/review","state":"opened","source_branch":"feat","target_branch":"develop"}]`)
	})
	client := newTestClient(t, "gitlab", "gitlab.test", "owner/repo", "secret", handlerClient(handler))
	if _, err := client.FindOpen(context.Background(), "feat", "develop"); err == nil {
		t.Fatal("expected invalid DTO error")
	}
}

func TestClientRejectsCrossHostReviewURL(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, `[{"iid":4,"web_url":"https://other.test/mr/4","state":"opened","title":"Unsafe URL","source_branch":"feat","target_branch":"develop"}]`)
	})
	client := newTestClient(t, "gitlab", "gitlab.test", "owner/repo", "secret", handlerClient(handler))
	_, err := client.FindOpen(context.Background(), "feat", "develop")
	if err == nil || !strings.Contains(err.Error(), "does not match repository host") {
		t.Fatalf("expected cross-host review URL rejection, got %v", err)
	}
}

func TestInjectedHTTPClientStillUsesSafetyTimeout(t *testing.T) {
	tests := []struct {
		provider string
		host     string
	}{
		{provider: "gitea", host: "gitea.test"},
		{provider: "gitlab", host: "gitlab.test"},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			client := newTestClient(t, test.provider, test.host, "owner/repo", "secret", handlerClient(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
			var httpClient *http.Client
			switch value := client.(type) {
			case *giteaClient:
				httpClient = value.httpClient
			case *gitLabClient:
				httpClient = value.httpClient
			default:
				t.Fatalf("unexpected client type %T", client)
			}
			if httpClient.Timeout != requestTimeout || httpClient.CheckRedirect == nil {
				t.Fatalf("injected client bypassed safety defaults: %#v", httpClient)
			}
		})
	}
}

func TestGiteaPrivateHTTPAuthorizationAndResponseURL(t *testing.T) {
	host := "127.0.0.1:3000"
	repository := hosting.Resolve("gitea", "http://"+host+"/owner/repo.git")
	repository.AllowInsecureHTTP = true
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "token http-secret" {
			t.Errorf("missing HTTP Gitea authorization header")
		}
		fmt.Fprint(writer, `[{"id":9,"number":9,"html_url":"http://127.0.0.1:3000/owner/repo/pulls/9","state":"open","title":"HTTP","head":{"ref":"feat/http","sha":"head"},"base":{"ref":"develop"}}]`)
	})
	client, err := NewClientWithOptions(repository, Options{
		HTTPClient: handlerClient(handler),
		APIBaseURL: "http://" + host,
		Getenv: environment(map[string]string{
			"KIT_GITEA_TOKEN": "http-secret",
			"KIT_GITEA_HOST":  host,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	found, err := client.FindOpen(context.Background(), "feat/http", "develop")
	if err != nil || len(found) != 1 || found[0].URL != "http://127.0.0.1:3000/owner/repo/pulls/9" {
		t.Fatalf("unexpected HTTP Gitea response: %#v, %v", found, err)
	}
}

func TestReviewClientRejectsUnsafeHTTPRepositoryOrigins(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		remote   string
		optIn    bool
	}{
		{name: "flag missing", provider: "gitea", remote: "http://10.0.0.2/owner/repo.git"},
		{name: "hostname", provider: "gitea", remote: "http://gitea.lan/owner/repo.git", optIn: true},
		{name: "public IP", provider: "gitea", remote: "http://8.8.8.8/owner/repo.git", optIn: true},
		{name: "GitLab", provider: "gitlab", remote: "http://10.0.0.2/owner/repo.git", optIn: true},
		{name: "Forgejo", provider: "forgejo", remote: "http://10.0.0.2/owner/repo.git", optIn: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := hosting.Resolve(test.provider, test.remote)
			repository.AllowInsecureHTTP = test.optIn
			if _, err := NewClientWithOptions(repository, Options{Getenv: environment(nil)}); err == nil {
				t.Fatalf("unsafe HTTP origin accepted: %#v", repository)
			}
		})
	}
}

func TestPrivateHTTPOriginCannotBeChangedByBaseRedirectOrResponse(t *testing.T) {
	host := "10.0.0.2:3000"
	repository := hosting.Resolve("gitea", "http://"+host+"/owner/repo.git")
	repository.AllowInsecureHTTP = true
	environment := environment(map[string]string{"KIT_GITEA_TOKEN": "secret", "KIT_GITEA_HOST": host})

	for _, baseURL := range []string{"https://" + host, "http://10.0.0.3:3000"} {
		if _, err := NewClientWithOptions(repository, Options{APIBaseURL: baseURL, Getenv: environment}); err == nil {
			t.Fatalf("mismatched API base accepted: %s", baseURL)
		}
	}

	for _, location := range []string{"https://" + host + "/review", "http://10.0.0.3:3000/review"} {
		handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, location, http.StatusFound)
		})
		client, err := NewClientWithOptions(repository, Options{HTTPClient: handlerClient(handler), Getenv: environment})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.FindOpen(context.Background(), "feat", "develop"); err == nil || !strings.Contains(err.Error(), "redirect changed") {
			t.Fatalf("redirect %s was not rejected: %v", location, err)
		}
	}

	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprint(writer, `[{"id":9,"number":9,"html_url":"https://10.0.0.2:3000/owner/repo/pulls/9","state":"open","title":"Wrong scheme","head":{"ref":"feat","sha":"head"},"base":{"ref":"develop"}}]`)
	})
	client, err := NewClientWithOptions(repository, Options{HTTPClient: handlerClient(handler), Getenv: environment})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.FindOpen(context.Background(), "feat", "develop"); err == nil || !strings.Contains(err.Error(), "host and scheme") {
		t.Fatalf("cross-scheme response URL was not rejected: %v", err)
	}
}

func TestHTTPSRequestToHTTPServerProvidesConfigurationGuidanceWithoutRetry(t *testing.T) {
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("http: server gave HTTP response to HTTPS client")
	})
	client := newTestClient(t, "gitea", "gitea.test", "owner/repo", "secret", &http.Client{Transport: transport})
	_, err := client.FindOpen(context.Background(), "feat", "develop")
	if err == nil || !strings.Contains(err.Error(), "git.allow-insecure-http") || !strings.Contains(err.Error(), "HTTPS reverse proxy") {
		t.Fatalf("missing HTTPS/HTTP guidance: %v", err)
	}
	if calls != 1 {
		t.Fatalf("request was retried or downgraded: %d calls", calls)
	}
}

func newTestClient(t *testing.T, provider, host, repositoryPath, token string, httpClient *http.Client) Client {
	t.Helper()
	baseURL := "https://" + host
	repository := hosting.Resolve(provider, baseURL+"/"+repositoryPath+".git")
	prefix := "GITLAB"
	if provider == "gitea" {
		prefix = "GITEA"
	} else if provider == "forgejo" {
		prefix = "FORGEJO"
	}
	client, err := NewClientWithOptions(repository, Options{
		HTTPClient: httpClient,
		APIBaseURL: baseURL,
		Getenv: environment(map[string]string{
			"KIT_" + prefix + "_TOKEN": token,
			"KIT_" + prefix + "_HOST":  host,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func handlerClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})}
}

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func forgejoJSON(merged bool, state string) string {
	return fmt.Sprintf(`{"id":99,"number":7,"html_url":"https://forgejo.test/owner/repo/pulls/7","state":%q,"merged":%t,"title":"Login","head":{"ref":"feat/login","sha":"head-sha"},"base":{"ref":"develop","sha":"base-sha"},"merge_commit_sha":"merge-sha"}`, state, merged)
}

func giteaJSON(merged bool, state, title string) string {
	mergedAt := "null"
	headRef := "feat/login"
	if merged {
		mergedAt = fmt.Sprintf("%q", time.Now().UTC().Truncate(time.Second).Format(time.RFC3339))
		headRef = "refs/pull/7/head"
	}
	return fmt.Sprintf(`{"id":99,"number":7,"html_url":"https://gitea.test/owner/repo/pulls/7","state":%q,"merged":%t,"title":%q,"head":{"label":"owner:feat/login","ref":%q,"sha":"head-sha"},"base":{"label":"owner:develop","ref":"develop","sha":"base-sha"},"merge_commit_sha":"merge-sha","merged_at":%s}`, state, merged, title, headRef, mergedAt)
}

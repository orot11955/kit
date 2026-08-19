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

	repository := hosting.Resolve("gitlab", "git@gitlab.example.com:owner/repo.git")
	_, err = NewClientWithOptions(repository, Options{Getenv: environment(map[string]string{
		"KIT_GITLAB_TOKEN": "secret-token",
		"KIT_GITLAB_HOST":  "other.example.com",
	})})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("expected safe host mismatch error, got %v", err)
	}
}

func TestNewClientRequiresExactLowercaseConfiguredHost(t *testing.T) {
	repository := hosting.Resolve("gitlab", "git@gitlab.example.com:owner/repo.git")
	_, err := NewClientWithOptions(repository, Options{Getenv: environment(map[string]string{
		"KIT_GITLAB_TOKEN": "secret-token",
		"KIT_GITLAB_HOST":  "GitLab.Example.com",
	})})
	if err == nil || !strings.Contains(err.Error(), "exact lowercase") {
		t.Fatalf("expected lowercase host error, got %v", err)
	}
}

func TestNewClientRejectsInconsistentRepositoryCoordinates(t *testing.T) {
	repository := hosting.Repository{
		Provider: "gitlab",
		Host:     "gitlab.example.com",
		Path:     "owner/../other/repo",
		Owner:    "owner/../other",
		Name:     "repo",
	}
	_, err := NewClientWithOptions(repository, Options{Getenv: environment(map[string]string{
		"KIT_GITLAB_TOKEN": "secret",
		"KIT_GITLAB_HOST":  "gitlab.example.com",
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
	client := newTestClient(t, "gitlab", "gitlab.test", "owner/repo", "secret", handlerClient(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	gitlab := client.(*gitLabClient)
	if gitlab.httpClient.Timeout != requestTimeout || gitlab.httpClient.CheckRedirect == nil {
		t.Fatalf("injected client bypassed safety defaults: %#v", gitlab.httpClient)
	}
}

func newTestClient(t *testing.T, provider, host, repositoryPath, token string, httpClient *http.Client) Client {
	t.Helper()
	baseURL := "https://" + host
	repository := hosting.Resolve(provider, baseURL+"/"+repositoryPath+".git")
	prefix := "GITLAB"
	if provider == "forgejo" {
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

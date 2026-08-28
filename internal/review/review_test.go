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

	"kit/internal/hosting"
)

func TestNewClientRequiresSafeGiteaCredentialBinding(t *testing.T) {
	repository := hosting.Resolve("gitea", "git@git.example.com:owner/repo.git")
	_, err := NewClientWithOptions(repository, Options{Getenv: environment(map[string]string{
		"KIT_GITEA_TOKEN": "secret-token", "KIT_GITEA_HOST": "other.example.com",
	})})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("expected safe host mismatch error, got %v", err)
	}
	lookups := 0
	client, err := NewClientWithOptions(repository, Options{Getenv: environment(nil), Lookup: func(provider, host string) (string, error) {
		lookups++
		if provider != "gitea" || host != "git.example.com" {
			t.Fatalf("unexpected lookup: %s %s", provider, host)
		}
		return "stored-secret", nil
	}})
	if err != nil || lookups != 1 || client == nil {
		t.Fatalf("stored credential was not used: client=%T lookups=%d err=%v", client, lookups, err)
	}
}

func TestCreateUsesProviderPayloads(t *testing.T) {
	tests := []struct {
		provider, host, repositoryPath, response string
		check                                    func(*testing.T, *http.Request)
	}{
		{"gitlab", "gitlab.test", "group/subgroup/repo", `{"id":900,"iid":42,"web_url":"https://gitlab.test/mr/42","state":"opened","title":"Login","source_branch":"feat/login","target_branch":"develop","sha":"source-sha"}`, func(t *testing.T, r *http.Request) {
			if r.Method != http.MethodPost || strings.Join(r.Header.Values("Private-Token"), ",") != "secret" || !strings.Contains(r.URL.EscapedPath(), "group%2Fsubgroup%2Frepo") {
				t.Fatalf("unexpected GitLab request: %s %s", r.Method, r.URL)
			}
			var p map[string]any
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p["source_branch"] != "feat/login" || p["remove_source_branch"] != true {
				t.Fatalf("unexpected GitLab payload: %#v", p)
			}
		}},
		{"gitea", "gitea.test", "owner/repo", `{"id":7,"number":7,"html_url":"https://gitea.test/owner/repo/pulls/7","state":"open","title":"WIP: Login","head":{"ref":"feat/login","sha":"head-sha"},"base":{"ref":"develop"}}`, func(t *testing.T, r *http.Request) {
			if r.Method != http.MethodPost || strings.Join(r.Header.Values("Authorization"), ",") != "token secret" {
				t.Fatalf("unexpected Gitea request: %s", r.Method)
			}
			var p map[string]any
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p["head"] != "feat/login" || p["base"] != "develop" || p["title"] != "WIP: Login" {
				t.Fatalf("unexpected Gitea payload: %#v", p)
			}
			if _, ok := p["draft"]; ok {
				t.Fatalf("Gitea payload must not contain draft: %#v", p)
			}
		}},
		{"forgejo", "forgejo.test", "owner/repo", `{"id":7,"number":7,"html_url":"https://forgejo.test/owner/repo/pulls/7","state":"open","title":"Login","head":{"ref":"feat/login","sha":"head-sha"},"base":{"ref":"develop"}}`, func(t *testing.T, r *http.Request) {
			var p map[string]any
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Fatal(err)
			}
			if r.Method != http.MethodPost || strings.Join(r.Header.Values("Authorization"), ",") != "token secret" || p["draft"] != true {
				t.Fatalf("unexpected Forgejo request/payload: %s %#v", r.Method, p)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			client := newTestClient(t, test.provider, test.host, test.repositoryPath, "secret", handlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { test.check(t, r); fmt.Fprint(w, test.response) })))
			created, err := client.Create(context.Background(), CreateRequest{SourceBranch: "feat/login", TargetBranch: "develop", Title: "Login", Draft: true, RemoveSourceBranch: true})
			if err != nil || created.ID == "" || created.URL == "" || created.SourceBranch != "feat/login" {
				t.Fatalf("unexpected created review: %#v, %v", created, err)
			}
		})
	}
}

func TestCreatePreservesHTTPBoundarySafety(t *testing.T) {
	request := CreateRequest{SourceBranch: "feat", TargetBranch: "develop", Title: "Title"}
	tests := []struct {
		name    string
		handler http.Handler
		want    string
		absent  []string
	}{
		{"cross-origin redirect", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://other.test/review", http.StatusFound)
		}), "redirect changed", nil},
		{"response limit", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(maxResponseBytes+1))
			w.WriteHeader(http.StatusOK)
		}), "size limit", nil},
		{"masked error body", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "server-secret-body", http.StatusUnauthorized)
		}), "HTTP 401", []string{"server-secret-body", "secret-token"}},
		{"response origin", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"iid":4,"web_url":"https://other.test/mr/4","state":"opened","title":"Unsafe URL","source_branch":"feat","target_branch":"develop"}`)
		}), "does not match repository host", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, "gitlab", "gitlab.test", "owner/repo", "secret-token", handlerClient(test.handler))
			_, err := client.Create(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
			for _, unsafe := range test.absent {
				if strings.Contains(err.Error(), unsafe) {
					t.Fatalf("unsafe API error: %v", err)
				}
			}
		})
	}
}

func TestGiteaPrivateHTTPCreateKeepsAuthorizationAndOrigin(t *testing.T) {
	host := "127.0.0.1:3000"
	repository := hosting.Resolve("gitea", "http://"+host+"/owner/repo.git")
	repository.AllowInsecureHTTP = true
	client, err := NewClientWithOptions(repository, Options{
		HTTPClient: handlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Join(r.Header.Values("Authorization"), ",") != "token http-secret" || r.Method != http.MethodPost {
				t.Fatalf("unexpected HTTP Gitea request: %s", r.Method)
			}
			fmt.Fprint(w, `{"id":9,"number":9,"html_url":"http://127.0.0.1:3000/owner/repo/pulls/9","state":"open","title":"HTTP","head":{"ref":"feat/http","sha":"head"},"base":{"ref":"develop"}}`)
		})), APIBaseURL: "http://" + host, Getenv: environment(map[string]string{"KIT_GITEA_TOKEN": "http-secret", "KIT_GITEA_HOST": host}),
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.Create(context.Background(), CreateRequest{SourceBranch: "feat/http", TargetBranch: "develop", Title: "HTTP"})
	if err != nil || created.URL != "http://127.0.0.1:3000/owner/repo/pulls/9" {
		t.Fatalf("unexpected HTTP Gitea response: %#v, %v", created, err)
	}
}

func TestCreateRejectsInvalidResponseDTO(t *testing.T) {
	client := newTestClient(t, "gitlab", "gitlab.test", "owner/repo", "secret", handlerClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"iid":0,"web_url":"http://unsafe.example/review","state":"opened","source_branch":"feat","target_branch":"develop"}`)
	})))
	if _, err := client.Create(context.Background(), CreateRequest{SourceBranch: "feat", TargetBranch: "develop", Title: "Title"}); err == nil {
		t.Fatal("expected invalid DTO error")
	}
}

func TestNewClientRejectsUnsupportedProvider(t *testing.T) {
	_, err := NewClientWithOptions(hosting.Resolve("generic", "git@example.com:owner/repo.git"), Options{})
	if !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("expected unsupported provider, got %v", err)
	}
}

func newTestClient(t *testing.T, provider, host, repositoryPath, token string, httpClient *http.Client) Client {
	t.Helper()
	prefix := strings.ToUpper(provider)
	client, err := NewClientWithOptions(hosting.Resolve(provider, "https://"+host+"/"+repositoryPath+".git"), Options{HTTPClient: httpClient, APIBaseURL: "https://" + host, Getenv: environment(map[string]string{"KIT_" + prefix + "_TOKEN": token, "KIT_" + prefix + "_HOST": host})})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func environment(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func handlerClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		response := recorder.Result()
		response.Request = r
		return response, nil
	})}
}

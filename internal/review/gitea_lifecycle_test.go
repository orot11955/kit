package review

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestGiteaFindOpenAndGet(t *testing.T) {
	client := newTestClient(t, "gitea", "gitea.test", "owner/repo", "secret", handlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Join(r.Header.Values("Authorization"), ",") != "token secret" {
			t.Fatalf("missing authorization: %#v", r.Header)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/owner/repo/pulls" && r.URL.Query().Get("state") == "open":
			fmt.Fprint(w, `[{"id":7,"number":7,"html_url":"https://gitea.test/owner/repo/pulls/7","state":"open","title":"Login","head":{"ref":"feat/login","sha":"head-sha"},"base":{"ref":"develop"}},{"id":8,"number":8,"html_url":"https://gitea.test/owner/repo/pulls/8","state":"open","title":"Other","head":{"ref":"feat/other","sha":"other-sha"},"base":{"ref":"develop"}}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/owner/repo/pulls/7":
			fmt.Fprint(w, `{"id":7,"number":7,"html_url":"https://gitea.test/owner/repo/pulls/7","state":"closed","merged":true,"title":"Login","head":{"ref":"feat/login","sha":"head-sha"},"base":{"ref":"develop"},"merge_commit_sha":"merge-sha"}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	})))

	finder, ok := client.(OpenFinder)
	if !ok {
		t.Fatalf("Gitea client does not implement OpenFinder: %T", client)
	}
	found, exists, err := finder.FindOpen(context.Background(), "feat/login", "develop")
	if err != nil || !exists || found.Number != 7 || found.Status != StatusOpen {
		t.Fatalf("unexpected open review: %#v exists=%v err=%v", found, exists, err)
	}

	getter, ok := client.(Getter)
	if !ok {
		t.Fatalf("Gitea client does not implement Getter: %T", client)
	}
	current, err := getter.Get(context.Background(), 7)
	if err != nil || current.Status != StatusMerged || current.MergeSHA != "merge-sha" {
		t.Fatalf("unexpected current review: %#v err=%v", current, err)
	}
}

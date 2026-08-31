package review

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestGiteaForkCreateAndFindOpen(t *testing.T) {
	client := newTestClient(t, "gitea", "gitea.test", "upstream/repo", "secret", handlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/upstream/repo/pulls":
			fmt.Fprint(w, `[{"id":7,"number":7,"html_url":"https://gitea.test/upstream/repo/pulls/7","state":"open","title":"Fork Login","head":{"label":"forker:feat/login","ref":"feat/login","sha":"fork-sha"},"base":{"label":"upstream:develop","ref":"develop"}},{"id":8,"number":8,"html_url":"https://gitea.test/upstream/repo/pulls/8","state":"open","title":"Other Fork","head":{"label":"other:feat/login","ref":"feat/login","sha":"other-sha"},"base":{"label":"upstream:develop","ref":"develop"}}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/upstream/repo/pulls":
			var body struct {
				Head string `json:"head"`
				Base string `json:"base"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Head != "forker:feat/new" || body.Base != "develop" {
				t.Fatalf("unexpected fork create payload: %#v", body)
			}
			fmt.Fprint(w, `{"id":9,"number":9,"html_url":"https://gitea.test/upstream/repo/pulls/9","state":"open","title":"New","head":{"label":"forker:feat/new","ref":"feat/new","sha":"new-sha"},"base":{"label":"upstream:develop","ref":"develop"}}`)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	})))

	fork, ok := client.(ForkClient)
	if !ok {
		t.Fatalf("Gitea client does not implement ForkClient: %T", client)
	}
	found, exists, err := fork.FindOpenFrom(context.Background(), "forker", "feat/login", "develop")
	if err != nil || !exists || found.Number != 7 || found.SourceBranch != "feat/login" {
		t.Fatalf("unexpected fork review: %#v exists=%v err=%v", found, exists, err)
	}
	created, err := fork.CreateFrom(context.Background(), "forker", CreateRequest{SourceBranch: "feat/new", TargetBranch: "develop", Title: "New"})
	if err != nil || created.Number != 9 || created.SourceSHA != "new-sha" {
		t.Fatalf("unexpected created fork review: %#v err=%v", created, err)
	}
}

func TestGiteaForkOwnerValidation(t *testing.T) {
	for _, value := range []string{"", "a/b", "a:b", "a b", "a@b", ".."} {
		if err := validateForkOwner(value); err == nil {
			t.Fatalf("expected invalid fork owner %q", value)
		}
	}
	if err := validateForkOwner("dev-user"); err != nil {
		t.Fatalf("valid owner rejected: %v", err)
	}
}

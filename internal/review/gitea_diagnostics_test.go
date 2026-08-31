package review

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestGiteaPingUsesReadOnlyRepositoryEndpoint(t *testing.T) {
	client := newTestClient(t, "gitea", "gitea.test", "owner/repo", "secret", handlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/owner/repo" {
			t.Fatalf("unexpected ping request: %s %s", r.Method, r.URL.Path)
		}
		if strings.Join(r.Header.Values("Authorization"), ",") != "token secret" {
			t.Fatalf("unexpected authorization header")
		}
		fmt.Fprint(w, `{"id":1}`)
	})))
	pinger, ok := client.(Pinger)
	if !ok {
		t.Fatalf("Gitea client does not expose Pinger: %T", client)
	}
	if err := pinger.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

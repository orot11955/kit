package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"kit/internal/buildinfo"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
	"kit/internal/review"
)

type doctorNetworkRunner struct{}

func (doctorNetworkRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "ls-remote" {
		ref := args[len(args)-1]
		if strings.HasSuffix(ref, "/work") {
			return []byte{}, nil
		}
		hash := strings.Repeat("a", 40)
		return []byte(fmt.Sprintf("%s\t%s\n", hash, ref)), nil
	}
	return (gitservice.ExecRunner{}).Run(ctx, dir, args...)
}

func (doctorNetworkRunner) RunInput(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error) {
	return (gitservice.ExecRunner{}).RunInput(ctx, dir, input, args...)
}

type doctorReviewClient struct{ pinged bool }

func (c *doctorReviewClient) Create(_ context.Context, request review.CreateRequest) (review.Review, error) {
	return review.Review{}, nil
}

func (c *doctorReviewClient) Ping(_ context.Context) error {
	c.pinged = true
	return nil
}

func TestDoctorNetworkJSONAndVerboseDiagnostics(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "branch", "main", "develop")
	gitCommand(t, dir, "branch", "work", "develop")
	gitCommand(t, dir, "remote", "add", "origin", "https://gitea.example/org/repo.git")

	client := &doctorReviewClient{}
	var output, diagnostics bytes.Buffer
	app := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &diagnostics},
		Build: buildinfo.Current(),
		Git: func(path string) gitservice.Service {
			return gitservice.Service{Dir: path, Runner: doctorNetworkRunner{}}
		},
		ReviewClient: func(repository hosting.Repository) (review.Client, error) {
			if repository.Provider != "gitea" || repository.Host != "gitea.example" {
				t.Fatalf("unexpected review repository: %#v", repository)
			}
			return client, nil
		},
	}
	if err := app.Run(context.Background(), []string{"doctor", "--network", "--verbose", "--json", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	var result doctorResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode doctor JSON: %v\n%s", err, output.String())
	}
	if !result.OK || !client.pinged {
		t.Fatalf("unexpected doctor result: ok=%v pinged=%v checks=%#v", result.OK, client.pinged, result.Checks)
	}
	checks := make(map[string]doctorCheck, len(result.Checks))
	for _, check := range result.Checks {
		checks[check.Name] = check
	}
	for _, name := range []string{"remote stable", "remote base", "remote source", "review API"} {
		if check, ok := checks[name]; !ok || !check.OK {
			t.Fatalf("network check %q failed or missing: %#v", name, check)
		}
	}
	trace := diagnostics.String()
	if !strings.Contains(trace, "+ git ls-remote") || !strings.Contains(trace, "+ kit network review ping gitea org/repo") {
		t.Fatalf("verbose diagnostics missing expected trace:\n%s", trace)
	}
}

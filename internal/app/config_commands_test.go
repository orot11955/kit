package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	gitservice "kit/internal/git"
	"kit/internal/hosting"
)

func TestPrintWorkflowConfigIncludesInsecureHTTPOptIn(t *testing.T) {
	config := gitservice.DefaultWorkflowConfig()
	config.AllowInsecureHTTP = true

	var textOutput bytes.Buffer
	application := &Application{IO: IO{Out: &textOutput}}
	if err := application.printWorkflowConfig(config, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textOutput.String(), "git.allow-insecure-http=true") {
		t.Fatalf("text config omitted opt-in: %s", textOutput.String())
	}

	var jsonOutput bytes.Buffer
	application.IO.Out = &jsonOutput
	if err := application.printWorkflowConfig(config, true); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(jsonOutput.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["allow_insecure_http"] != true {
		t.Fatalf("JSON config omitted opt-in: %#v", decoded)
	}
}

func TestReviewRepositoryCarriesRemoteSchemeAndOptIn(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "remote", "add", "origin", "http://10.0.0.20:3000/owner/repo.git")
	config := gitservice.DefaultWorkflowConfig()
	config.AllowInsecureHTTP = true
	repository, err := reviewRepository(context.Background(), gitservice.Service{Dir: dir}, config)
	if err != nil {
		t.Fatal(err)
	}
	if repository.Scheme != "http" || !repository.AllowInsecureHTTP || !repository.InsecureHTTPAllowed() {
		t.Fatalf("review transport policy was not preserved: %#v", repository)
	}
}

func TestInsecureHTTPWarningIsWrittenOnceToErrorOutput(t *testing.T) {
	repository := hosting.Resolve("gitea", "http://127.0.0.1:3000/owner/repo.git")
	repository.AllowInsecureHTTP = true
	ctx := withInsecureHTTPWarningOnce(context.Background())
	ctx = withInsecureHTTPWarningOnce(ctx)
	var stdout, stderr bytes.Buffer
	warnInsecureHTTP(ctx, &stderr, repository)
	warnInsecureHTTP(ctx, &stderr, repository)
	if count := strings.Count(stderr.String(), insecureHTTPWarning); count != 1 {
		t.Fatalf("warning count=%d, output=%q", count, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("warning polluted stdout: %q", stdout.String())
	}
}

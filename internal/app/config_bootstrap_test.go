package app

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
)

func TestConfigBootstrapCreatesMissingLocalWorkflowBranches(t *testing.T) {
	source := appRepository(t)
	gitCommand(t, source, "branch", "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, source, "remote", "add", "origin", remote)
	gitCommand(t, source, "push", "origin", "develop", "main")

	clone := filepath.Join(t.TempDir(), "clone")
	gitCommand(t, "", "clone", "--branch", "develop", remote, clone)

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}}
	if err := a.Run(context.Background(), []string{"config", "bootstrap", "--cwd", clone, "--json"}); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"main", "develop", "work"} {
		if !commandSucceeds(clone, "show-ref", "--verify", "--quiet", "refs/heads/"+branch) {
			t.Fatalf("bootstrap did not create/retain local branch %s", branch)
		}
	}
	if got, want := gitCommandOutput(t, clone, "rev-parse", "work"), gitCommandOutput(t, clone, "rev-parse", "develop"); got != want {
		t.Fatalf("work was not created from develop: got %s want %s", got, want)
	}
	if commandSucceeds(clone, "show-ref", "--verify", "--quiet", "refs/remotes/origin/work") {
		t.Fatal("bootstrap unexpectedly created a remote work branch")
	}
	config := (gitservice.Service{Dir: clone}).WorkflowConfig(context.Background())
	if config.Remote != "origin" || config.Stable != "main" || config.Base != "develop" || config.Source != "work" || config.Provider != "gitea" {
		t.Fatalf("unexpected workflow config: %#v", config)
	}
}

func TestConfigBootstrapRejectsRemoteSourceQueue(t *testing.T) {
	source := appRepository(t)
	gitCommand(t, source, "branch", "main")
	gitCommand(t, source, "branch", "work")
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, source, "remote", "add", "origin", remote)
	gitCommand(t, source, "push", "origin", "develop", "main", "work")

	clone := filepath.Join(t.TempDir(), "clone")
	gitCommand(t, "", "clone", "--branch", "develop", remote, clone)

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}}
	err := a.Run(context.Background(), []string{"config", "bootstrap", "--cwd", clone})
	if clierror.Code(err) != clierror.Conflict || !strings.Contains(err.Error(), "local-only queue") {
		t.Fatalf("expected remote source queue conflict, got %v", err)
	}
	if commandSucceeds(clone, "show-ref", "--verify", "--quiet", "refs/heads/work") {
		t.Fatal("bootstrap created local work even though origin/work exists")
	}
}

func TestConfigBootstrapRejectsStaleExistingSource(t *testing.T) {
	source := appRepository(t)
	gitCommand(t, source, "branch", "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, source, "remote", "add", "origin", remote)
	gitCommand(t, source, "push", "origin", "develop", "main")

	clone := filepath.Join(t.TempDir(), "clone")
	gitCommand(t, "", "clone", "--branch", "develop", remote, clone)
	gitCommand(t, clone, "config", "user.name", "Kit Test")
	gitCommand(t, clone, "config", "user.email", "kit@example.invalid")
	gitCommand(t, clone, "switch", "--orphan", "work")
	gitCommand(t, clone, "commit", "--allow-empty", "-m", "unrelated work")
	gitCommand(t, clone, "switch", "develop")

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}}
	err := a.Run(context.Background(), []string{"config", "bootstrap", "--cwd", clone})
	if clierror.Code(err) != clierror.Conflict || !strings.Contains(err.Error(), "kit sync") {
		t.Fatalf("expected stale source conflict, got %v", err)
	}
	if got := gitCommandOutput(t, clone, "log", "-1", "--format=%s", "work"); got != "unrelated work" {
		t.Fatalf("bootstrap overwrote stale work: %s", got)
	}
}

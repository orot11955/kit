package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kit/internal/buildinfo"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
	"kit/internal/pickstate"
	"kit/internal/review"
)

type recordingReviewClient struct {
	calls int
	err   error
}

func (c *recordingReviewClient) Create(_ context.Context, request review.CreateRequest) (review.Review, error) {
	c.calls++
	if c.err != nil {
		return review.Review{}, c.err
	}
	return review.Review{ID: "1", URL: "https://gitea.example/org/repo/pulls/1", SourceBranch: request.SourceBranch, TargetBranch: request.TargetBranch}, nil
}

func TestReviewSubmitCreateReturnsImmediatelyAndKeepsBranchOnFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "create failure", err: errors.New("provider unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := appRepository(t)
			remote := filepath.Join(t.TempDir(), "remote.git")
			gitCommand(t, "", "init", "--bare", remote)
			gitCommand(t, dir, "remote", "add", "origin", remote)
			gitCommand(t, dir, "switch", "-c", "feat/review")
			if err := os.WriteFile(filepath.Join(dir, "review.txt"), []byte("review\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCommand(t, dir, "add", "review.txt")
			gitCommand(t, dir, "commit", "-m", "review title")

			client := &recordingReviewClient{err: test.err}
			app := &Application{
				IO:           IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}},
				Build:        buildinfo.Current(),
				ReviewClient: func(hosting.Repository) (review.Client, error) { return client, nil },
			}
			err := app.Run(context.Background(), []string{"review", "submit", "--cwd", dir, "--yes"})
			if test.err == nil && err != nil {
				t.Fatal(err)
			}
			if test.err != nil && (err == nil || !strings.Contains(err.Error(), "verify the PR manually")) {
				t.Fatalf("expected retained-branch guidance, got %v", err)
			}
			if client.calls != 1 {
				t.Fatalf("Create calls=%d, want 1", client.calls)
			}
			if !commandSucceeds(dir, "show-ref", "--verify", "--quiet", "refs/heads/feat/review") {
				t.Fatal("review branch was removed after Create result")
			}
		})
	}
}

func TestDeprecatedReviewWaitAndFinishDoNotInitializeGitOrProvider(t *testing.T) {
	for _, command := range []string{"wait", "finish"} {
		t.Run(command, func(t *testing.T) {
			app := &Application{
				IO:    IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}},
				Build: buildinfo.Current(),
				Git: func(string) gitservice.Service {
					panic("Git must not be initialized by deprecated review command")
				},
				ReviewClient: func(hosting.Repository) (review.Client, error) {
					panic("provider must not be initialized by deprecated review command")
				},
			}
			if err := app.Run(context.Background(), []string{"review", command, "--json"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReadReviewDescriptionRejectsSymlinkAndOverOneMiB(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "description.md")
	if err := os.WriteFile(regular, []byte("description"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "description-link.md")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readReviewDescription(link); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink was accepted: %v", err)
	}
	oversized := filepath.Join(dir, "oversized.md")
	if err := os.WriteFile(oversized, make([]byte, (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReviewDescription(oversized); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("oversized description was accepted: %v", err)
	}
}

func TestReviewDescriptionPathReplacementHasDistinctFileIdentity(t *testing.T) {
	dir := t.TempDir()
	initialPath := filepath.Join(dir, "initial.md")
	openedPath := filepath.Join(dir, "opened.md")
	if err := os.WriteFile(initialPath, []byte("initial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(openedPath, []byte("opened"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := os.Lstat(initialPath)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := os.Stat(openedPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(initial, opened) {
		t.Fatal("different regular files must not pass the description open-FD identity check")
	}
}

func TestDefaultPickRollsBackWhenReviewClientInitializationFails(t *testing.T) {
	dir := appRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "push", "origin", "develop")
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "work.txt")
	gitCommand(t, dir, "commit", "-m", "work change")
	gitCommand(t, dir, "push", "origin", "work")
	gitCommand(t, dir, "switch", "develop")
	originalHash := gitCommandOutput(t, dir, "rev-parse", "HEAD")

	client := &recordingReviewClient{}
	app := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}},
		Build: buildinfo.Current(),
		ReviewClient: func(hosting.Repository) (review.Client, error) {
			return client, errors.New("credential unavailable")
		},
	}
	err := app.Run(context.Background(), []string{"pick", "feat/rollback", "--all", "--yes", "--cwd", dir})
	if err == nil || !strings.Contains(err.Error(), "restored develop and removed feat/rollback") {
		t.Fatalf("expected pre-push rollback result, got %v", err)
	}
	if branch := gitCommandOutput(t, dir, "branch", "--show-current"); branch != "develop" {
		t.Fatalf("checkout was not restored: %s", branch)
	}
	if head := gitCommandOutput(t, dir, "rev-parse", "HEAD"); head != originalHash {
		t.Fatalf("HEAD was not restored: got %s want %s", head, originalHash)
	}
	assertBranchMissing(t, dir, "feat/rollback")
	if marked, markerErr := (gitservice.Service{Dir: dir}).IsKitCreatedBranch(context.Background(), "feat/rollback"); markerErr != nil || marked {
		t.Fatalf("Kit branch marker remained after pre-push rollback: marked=%v err=%v", marked, markerErr)
	}
	if _, stateErr := pickstate.Load(context.Background(), gitservice.Service{Dir: dir}); !errors.Is(stateErr, pickstate.ErrNotFound) {
		t.Fatalf("pick state remained after rollback: %v", stateErr)
	}
	if commandSucceeds(remote, "show-ref", "--verify", "--quiet", "refs/heads/feat/rollback") {
		t.Fatal("target branch was pushed before provider initialization completed")
	}
	if client.calls != 0 {
		t.Fatalf("Create ran after provider initialization failed: %d", client.calls)
	}
}

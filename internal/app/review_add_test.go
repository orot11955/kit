package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kit/internal/buildinfo"
	"kit/internal/hosting"
	"kit/internal/review"
	"kit/internal/reviewstate"
)

type reviewAddClient struct{ item review.Review }

func (c *reviewAddClient) Create(_ context.Context, request review.CreateRequest) (review.Review, error) {
	return c.item, nil
}

func (c *reviewAddClient) Get(_ context.Context, number int64) (review.Review, error) {
	item := c.item
	item.Number = number
	item.ID = "7"
	return item, nil
}

func TestReviewAddAppendsPendingCommitAndRestoresCheckout(t *testing.T) {
	dir, remote, first, second, reviewTip := setupReviewAddRepository(t, false)
	client := &reviewAddClient{item: review.Review{
		Provider: "gitea", ID: "7", Number: 7,
		URL: "https://gitea.example/org/repo/pulls/7", Status: review.StatusOpen,
		SourceBranch: "feat/review", TargetBranch: "develop", Title: "Review", SourceSHA: reviewTip,
	}}
	var output bytes.Buffer
	app := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Current(),
		ReviewClient: func(hosting.Repository) (review.Client, error) { return client, nil },
	}
	if err := app.Run(context.Background(), []string{"review", "add", "feat/review", "--commit", second[:8], "--yes", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if branch := gitCommandOutput(t, dir, "branch", "--show-current"); branch != "work" {
		t.Fatalf("original checkout was not restored: %s", branch)
	}
	localTip := gitCommandOutput(t, dir, "rev-parse", "feat/review")
	remoteTip := gitCommandOutput(t, remote, "rev-parse", "refs/heads/feat/review")
	if localTip == reviewTip || remoteTip != localTip {
		t.Fatalf("review branch was not pushed correctly: before=%s local=%s remote=%s", reviewTip, localTip, remoteTip)
	}
	state, err := reviewstate.Load(context.Background(), app.GitServiceForTest(dir), "feat/review")
	if err != nil {
		t.Fatal(err)
	}
	if state.PublishedTip != localTip {
		t.Fatalf("published tip=%s, want %s", state.PublishedTip, localTip)
	}
	want := map[string]bool{strings.ToLower(first): false, strings.ToLower(second): false}
	for _, hash := range state.SourceCommits {
		if _, ok := want[strings.ToLower(hash)]; ok {
			want[strings.ToLower(hash)] = true
		}
	}
	for hash, found := range want {
		if !found {
			t.Fatalf("review state is missing source commit %s: %#v", hash, state.SourceCommits)
		}
	}
}

func TestReviewAddConflictRollsBackEntireAddition(t *testing.T) {
	dir, remote, _, second, reviewTip := setupReviewAddRepository(t, true)
	client := &reviewAddClient{item: review.Review{
		Provider: "gitea", ID: "7", Number: 7,
		URL: "https://gitea.example/org/repo/pulls/7", Status: review.StatusOpen,
		SourceBranch: "feat/review", TargetBranch: "develop", Title: "Review", SourceSHA: reviewTip,
	}}
	var output bytes.Buffer
	app := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Current(),
		ReviewClient: func(hosting.Repository) (review.Client, error) { return client, nil },
	}
	err := app.Run(context.Background(), []string{"review", "add", "feat/review", "--commit", second[:8], "--yes", "--cwd", dir})
	if err == nil || !strings.Contains(err.Error(), "restored feat/review") {
		t.Fatalf("expected transactional conflict rollback, got %v", err)
	}
	if branch := gitCommandOutput(t, dir, "branch", "--show-current"); branch != "work" {
		t.Fatalf("original checkout was not restored after conflict: %s", branch)
	}
	if got := gitCommandOutput(t, dir, "rev-parse", "feat/review"); got != reviewTip {
		t.Fatalf("local review branch moved after rollback: got %s want %s", got, reviewTip)
	}
	if got := gitCommandOutput(t, remote, "rev-parse", "refs/heads/feat/review"); got != reviewTip {
		t.Fatalf("remote review branch moved after rollback: got %s want %s", got, reviewTip)
	}
}

func setupReviewAddRepository(t *testing.T, conflict bool) (dir, remote, first, second, reviewTip string) {
	t.Helper()
	dir = appRepository(t)
	remote = filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "push", "-u", "origin", "develop")

	if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "conflict.txt")
	gitCommand(t, dir, "commit", "-m", "add conflict base")
	gitCommand(t, dir, "push", "origin", "develop")

	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "first.txt")
	gitCommand(t, dir, "commit", "-m", "first work")
	first = gitCommandOutput(t, dir, "rev-parse", "HEAD")
	if conflict {
		if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(filepath.Join(dir, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if conflict {
		gitCommand(t, dir, "add", "conflict.txt")
	} else {
		gitCommand(t, dir, "add", "second.txt")
	}
	gitCommand(t, dir, "commit", "-m", "second work")
	second = gitCommandOutput(t, dir, "rev-parse", "HEAD")

	gitCommand(t, dir, "switch", "develop")
	gitCommand(t, dir, "switch", "-c", "feat/review")
	gitCommand(t, dir, "cherry-pick", "-x", first)
	if conflict {
		if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("review\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCommand(t, dir, "add", "conflict.txt")
		gitCommand(t, dir, "commit", "-m", "review-only conflict")
	}
	service := app.GitServiceForTest(dir)
	if err := service.MarkKitCreatedBranch(context.Background(), "feat/review"); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "push", "-u", "origin", "feat/review")
	reviewTip = gitCommandOutput(t, dir, "rev-parse", "HEAD")
	state := reviewstate.State{
		Stage: reviewstate.StageOpen, Provider: "gitea", Remote: "origin",
		Branch: "feat/review", SourceBranch: "work", TargetBranch: "develop",
		SourceCommits: []string{first}, ReviewID: "7", ReviewNumber: 7,
		ReviewURL: "https://gitea.example/org/repo/pulls/7", Status: review.StatusOpen,
		PickedTip: reviewTip, PublishedTip: reviewTip,
	}
	if err := reviewstate.Save(context.Background(), service, state); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "switch", "work")
	return dir, remote, first, second, reviewTip
}

func (a *Application) GitServiceForTest(dir string) gitServiceAlias {
	return gitServiceAlias{Dir: dir}
}

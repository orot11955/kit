package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kit/internal/buildinfo"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
	"kit/internal/review"
	"kit/internal/reviewstate"
)

func TestReviewFinishReconcilesSquashMergedWorkAndCleansManagedBranch(t *testing.T) {
	dir := appRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "push", "origin", "develop")

	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "a.txt")
	gitCommand(t, dir, "commit", "-m", "add a")
	first := gitCommandOutput(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "b.txt")
	gitCommand(t, dir, "commit", "-m", "add b")
	second := gitCommandOutput(t, dir, "rev-parse", "HEAD")

	client := &recordingReviewClient{}
	var output bytes.Buffer
	app := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Current(),
		ReviewClient: func(hosting.Repository) (review.Client, error) {
			return client, nil
		},
	}
	if err := app.Run(context.Background(), []string{"pick", "feat/squash", "--all", "--yes", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	reviewTip := gitCommandOutput(t, dir, "rev-parse", "feat/squash")
	state, err := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, "feat/squash")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.SourceCommits) != 2 || state.SourceCommits[0] != first || state.SourceCommits[1] != second {
		t.Fatalf("review state did not preserve original work commits: %#v", state.SourceCommits)
	}

	gitCommand(t, dir, "switch", "develop")
	gitCommand(t, dir, "merge", "--squash", "feat/squash")
	gitCommand(t, dir, "commit", "-m", "squash review")
	mergeSHA := gitCommandOutput(t, dir, "rev-parse", "HEAD")
	gitCommand(t, dir, "push", "origin", "develop")
	client.current = review.Review{
		Provider: "gitea", ID: "1", Number: 1, URL: "https://gitea.example/org/repo/pulls/1",
		Status: review.StatusMerged, SourceBranch: "feat/squash", TargetBranch: "develop", Title: "add a",
		SourceSHA: reviewTip, MergeSHA: mergeSHA,
	}

	output.Reset()
	if err := app.Run(context.Background(), []string{"review", "finish", "feat/squash", "--yes", "--cwd", dir}); err != nil {
		t.Fatalf("review finish failed after squash merge: %v\n%s", err, output.String())
	}
	if got, want := gitCommandOutput(t, dir, "rev-parse", "work"), gitCommandOutput(t, dir, "rev-parse", "develop"); got != want {
		t.Fatalf("reviewed work commits remain after reconcile: work=%s develop=%s", got, want)
	}
	if commandSucceeds(dir, "show-ref", "--verify", "--quiet", "refs/heads/feat/squash") {
		t.Fatal("managed local review branch remained after finish")
	}
	if commandSucceeds(remote, "show-ref", "--verify", "--quiet", "refs/heads/feat/squash") {
		t.Fatal("managed remote review branch remained after finish")
	}
	state, err = reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, "feat/squash")
	if err != nil || state.Stage != reviewstate.StageCleaned || state.Status != review.StatusMerged {
		t.Fatalf("review state was not finalized: %#v err=%v", state, err)
	}
	if !strings.Contains(output.String(), "Reconcile") {
		t.Fatalf("finish output did not report reconcile: %s", output.String())
	}
}

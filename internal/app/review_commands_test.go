package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kit/internal/buildinfo"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
	"kit/internal/review"
	"kit/internal/reviewstate"
	"kit/internal/selector"
)

type fakeReviewClient struct {
	current     review.Review
	createCalls int
}

func (f *fakeReviewClient) FindOpen(_ context.Context, source, target string) ([]review.Review, error) {
	if f.current.ID != "" && f.current.Status == review.StatusOpen && f.current.SourceBranch == source && f.current.TargetBranch == target {
		return []review.Review{f.current}, nil
	}
	return nil, nil
}

func (f *fakeReviewClient) Find(_ context.Context, source, target string) ([]review.Review, error) {
	if f.current.ID != "" && f.current.SourceBranch == source && f.current.TargetBranch == target {
		return []review.Review{f.current}, nil
	}
	return nil, nil
}

func (f *fakeReviewClient) Create(_ context.Context, request review.CreateRequest) (review.Review, error) {
	f.createCalls++
	f.current = review.Review{
		Provider:     "gitlab",
		ID:           "17",
		Number:       17,
		URL:          "https://gitlab.example/group/project/-/merge_requests/17",
		Status:       review.StatusOpen,
		SourceBranch: request.SourceBranch,
		TargetBranch: request.TargetBranch,
		Title:        request.Title,
	}
	return f.current, nil
}

func (f *fakeReviewClient) Get(_ context.Context, id string) (review.Review, error) {
	if id != f.current.ID {
		return review.Review{}, fmt.Errorf("unknown review %s", id)
	}
	return f.current, nil
}

func reviewApplication(client review.Client, input io.Reader, output io.Writer) *Application {
	return &Application{
		IO:    IO{In: input, Out: output, ErrOut: output},
		Build: buildinfo.Current(),
		ReviewClient: func(hosting.Repository) (review.Client, error) {
			return client, nil
		},
	}
}

func TestReviewSubmitIsIdempotentAndTracksState(t *testing.T) {
	dir, remote := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "switch", "-c", "feat/review")
	if err := os.WriteFile(filepath.Join(dir, "review.txt"), []byte("review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "review.txt")
	gitCommand(t, dir, "commit", "-m", "feat: review submit")

	client := &fakeReviewClient{}
	var output bytes.Buffer
	a := reviewApplication(client, strings.NewReader(""), &output)
	description := filepath.Join(t.TempDir(), "review-description.md")
	if err := os.WriteFile(description, []byte("review body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := a.Run(context.Background(), []string{"git", "review", "submit", "--cwd", dir, "--yes", "--description-file", description}); err != nil {
			t.Fatalf("submit attempt %d: %v", attempt+1, err)
		}
		if attempt == 0 {
			if err := os.Remove(description); err != nil {
				t.Fatal(err)
			}
		}
	}
	if client.createCalls != 1 {
		t.Fatalf("created %d reviews, want exactly one", client.createCalls)
	}
	if !commandSucceeds(remote, "show-ref", "--verify", "--quiet", "refs/heads/feat/review") {
		t.Fatal("review branch was not pushed")
	}
	state, err := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, "feat/review")
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != reviewstate.StageOpen || state.ReviewNumber != 17 || state.PublishedTip == "" {
		t.Fatalf("unexpected review state: %#v", state)
	}
	if !strings.Contains(output.String(), "kit · review submit") || !strings.Contains(output.String(), "기존 리뷰 재사용") {
		t.Fatalf("missing unified/reuse output:\n%s", output.String())
	}
}

func TestReadReviewDescriptionRejectsSymlinkAndOversizedFile(t *testing.T) {
	directory := t.TempDir()
	regular := filepath.Join(directory, "description.md")
	if err := os.WriteFile(regular, []byte("safe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readReviewDescription(regular)
	if err != nil || string(data) != "safe\n" {
		t.Fatalf("regular description: %q, %v", data, err)
	}
	link := filepath.Join(directory, "description-link.md")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readReviewDescription(link); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
	large := filepath.Join(directory, "large.md")
	if err := os.WriteFile(large, make([]byte, (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReviewDescription(large); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size rejection, got %v", err)
	}
}

func TestReviewFinishRefusesOpenReview(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	client, state, app := submitSimpleReview(t, dir, "feat/open")
	err := app.Run(context.Background(), []string{"git", "review", "finish", state.Branch, "--cwd", dir, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "requires a merged review") {
		t.Fatalf("expected open review rejection, got %v", err)
	}
	if client.current.Status != review.StatusOpen || !commandSucceeds(dir, "show-ref", "--verify", "--quiet", "refs/heads/"+state.Branch) {
		t.Fatal("open review changed local state")
	}
}

func TestReviewStatusRecoversMergedReviewAfterCreateResponseLoss(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	client, state, app := submitSimpleReview(t, dir, "feat/recover")
	state.Stage = reviewstate.StagePublished
	state.ReviewID = ""
	state.ReviewNumber = 0
	state.ReviewURL = ""
	state.Status = ""
	if err := reviewstate.Save(context.Background(), gitservice.Service{Dir: dir}, state); err != nil {
		t.Fatal(err)
	}
	client.current.Status = review.StatusMerged
	client.current.SourceSHA = state.PublishedTip
	if err := app.Run(context.Background(), []string{"git", "review", "status", state.Branch, "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	recovered, err := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, state.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Stage != reviewstate.StageMerged || recovered.ReviewID == "" {
		t.Fatalf("merged review was not recovered: %#v", recovered)
	}
}

func TestReviewFinishChecksProviderHeadWithoutLocalBranch(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	client, state, app := submitSimpleReview(t, dir, "feat/removed")
	gitCommand(t, dir, "switch", "develop")
	gitCommand(t, dir, "branch", "-D", state.Branch)
	client.current.Status = review.StatusMerged
	client.current.SourceSHA = strings.Repeat("a", 40)
	err := app.Run(context.Background(), []string{"git", "review", "finish", state.Branch, "--cwd", dir, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "provider review head differs") {
		t.Fatalf("expected provider head mismatch, got %v", err)
	}
}

func TestReviewFinishRefusesWorkflowSourceDrift(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	client, state, app := submitSimpleReview(t, dir, "feat/config-drift")
	client.current.Status = review.StatusMerged
	client.current.SourceSHA = state.PublishedTip
	gitCommand(t, dir, "config", "kit.git.source", "other-work")
	err := app.Run(context.Background(), []string{"git", "review", "finish", state.Branch, "--cwd", dir, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "workflow changed since submit") {
		t.Fatalf("expected source drift rejection, got %v", err)
	}
}

func TestPickSubmitAndFinishSquashMergeKeepsOnlyPendingWork(t *testing.T) {
	dir, remote := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "switch", "-c", "work")
	for index, content := range []string{"one\n", "one\ntwo\n"} {
		if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCommand(t, dir, "add", "feature.txt")
		gitCommand(t, dir, "commit", "-m", fmt.Sprintf("feature part %d", index+1))
	}
	gitCommand(t, dir, "switch", "develop")

	client := &fakeReviewClient{}
	var output bytes.Buffer
	a := reviewApplication(client, strings.NewReader(""), &output)
	a.Select = func(items []selector.Item, _ string) ([]selector.Item, error) { return items, nil }
	if err := a.Run(context.Background(), []string{"pick", "feat/squash", "--cwd", dir, "--yes", "--submit"}); err != nil {
		t.Fatal(err)
	}
	state, err := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, "feat/squash")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.SourceCommits) != 2 || state.PublishedTip == "" {
		t.Fatalf("pick metadata was not preserved: %#v", state)
	}

	mergeReviewBranchAsSquash(t, remote, "feat/squash")
	mergedAt := time.Now().UTC()
	client.current.Status = review.StatusMerged
	client.current.SourceSHA = state.PublishedTip
	client.current.MergeSHA = gitCommandOutput(t, remote, "rev-parse", "refs/heads/develop")
	client.current.MergedAt = &mergedAt

	output.Reset()
	if err := a.Run(context.Background(), []string{"git", "review", "finish", "feat/squash", "--cwd", dir, "--yes", "--force-delete"}); err != nil {
		t.Fatal(err)
	}
	assertBranchMissing(t, dir, "feat/squash")
	if work, develop := gitCommandOutput(t, dir, "rev-parse", "work"), gitCommandOutput(t, dir, "rev-parse", "develop"); work != develop {
		t.Fatalf("merged source work remained after finish: work=%s develop=%s", work, develop)
	}
	state, err = reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, "feat/squash")
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != reviewstate.StageCleaned || state.SyncedAt == nil || state.CleanedAt == nil {
		t.Fatalf("review was not finalized: %#v", state)
	}
	if !strings.Contains(output.String(), "머지된 작업 2 제거 · 작업 0 유지") {
		t.Fatalf("finish output did not explain the simplified sync result:\n%s", output.String())
	}
}

func TestReviewFinishNormalMergeUsesSafeBranchDelete(t *testing.T) {
	dir, remote := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "normal.txt"), []byte("normal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "normal.txt")
	gitCommand(t, dir, "commit", "-m", "feat: normal merge")
	gitCommand(t, dir, "switch", "develop")

	client := &fakeReviewClient{}
	app := reviewApplication(client, strings.NewReader(""), io.Discard)
	app.Select = func(items []selector.Item, _ string) ([]selector.Item, error) { return items, nil }
	if err := app.Run(context.Background(), []string{"pick", "feat/normal", "--cwd", dir, "--yes", "--submit"}); err != nil {
		t.Fatal(err)
	}
	state, err := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, "feat/normal")
	if err != nil {
		t.Fatal(err)
	}
	mergeSHA := mergeReviewBranchNormally(t, remote, "feat/normal")
	client.current.Status = review.StatusMerged
	client.current.SourceSHA = state.PublishedTip
	client.current.MergeSHA = mergeSHA
	if err := app.Run(context.Background(), []string{"git", "review", "finish", "feat/normal", "--cwd", dir, "--yes"}); err != nil {
		t.Fatal(err)
	}
	assertBranchMissing(t, dir, "feat/normal")
}

func reviewRepositoryWithRemote(t *testing.T) (string, string) {
	t.Helper()
	dir := appRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, remote, "symbolic-ref", "HEAD", "refs/heads/develop")
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "config", "kit.git.provider", "gitlab")
	gitCommand(t, dir, "push", "-u", "origin", "develop")
	return dir, remote
}

func mergeReviewBranchAsSquash(t *testing.T, remote, branch string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "merge")
	gitCommand(t, "", "clone", remote, clone)
	gitCommand(t, clone, "config", "user.name", "Kit Merge Test")
	gitCommand(t, clone, "config", "user.email", "merge@example.invalid")
	gitCommand(t, clone, "fetch", "origin", branch)
	gitCommand(t, clone, "merge", "--squash", "origin/"+branch)
	gitCommand(t, clone, "commit", "-m", "feat: squash merged review")
	gitCommand(t, clone, "push", "origin", "develop")
}

func mergeReviewBranchNormally(t *testing.T, remote, branch string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "merge-normal")
	gitCommand(t, "", "clone", remote, clone)
	gitCommand(t, clone, "config", "user.name", "Kit Merge Test")
	gitCommand(t, clone, "config", "user.email", "merge@example.invalid")
	gitCommand(t, clone, "fetch", "origin", branch)
	gitCommand(t, clone, "merge", "--no-ff", "origin/"+branch, "-m", "merge review")
	gitCommand(t, clone, "push", "origin", "develop")
	return gitCommandOutput(t, clone, "rev-parse", "HEAD")
}

func submitSimpleReview(t *testing.T, dir, branch string) (*fakeReviewClient, reviewstate.State, *Application) {
	t.Helper()
	gitCommand(t, dir, "switch", "-c", branch)
	if err := os.WriteFile(filepath.Join(dir, strings.ReplaceAll(branch, "/", "-")+".txt"), []byte("review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", ".")
	gitCommand(t, dir, "commit", "-m", "feat: review safety")
	client := &fakeReviewClient{}
	app := reviewApplication(client, strings.NewReader(""), io.Discard)
	if err := app.Run(context.Background(), []string{"git", "review", "submit", "--cwd", dir, "--yes"}); err != nil {
		t.Fatal(err)
	}
	state, err := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, branch)
	if err != nil {
		t.Fatal(err)
	}
	return client, state, app
}

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
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
	current        review.Review
	createCalls    int
	lastCreate     review.CreateRequest
	findOpenErrors []error
	getErrors      []error
	getCalls       int
	blockGet       bool
}

func (f *fakeReviewClient) FindOpen(_ context.Context, source, target string) ([]review.Review, error) {
	if len(f.findOpenErrors) > 0 {
		err := f.findOpenErrors[0]
		f.findOpenErrors = f.findOpenErrors[1:]
		return nil, err
	}
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
	f.lastCreate = request
	f.current = review.Review{
		Provider:     "gitea",
		ID:           "17",
		Number:       17,
		URL:          "https://gitea.example/group/project/pulls/17",
		Status:       review.StatusOpen,
		SourceBranch: request.SourceBranch,
		TargetBranch: request.TargetBranch,
		Title:        request.Title,
	}
	return f.current, nil
}

func TestReviewSubmitPassesGiteaDraftIntentAndPrintsGiteaLabel(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "switch", "-c", "feat/draft")
	if err := os.WriteFile(filepath.Join(dir, "draft.txt"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "draft.txt")
	gitCommand(t, dir, "commit", "-m", "feat: draft")

	client := &fakeReviewClient{}
	var output bytes.Buffer
	app := reviewApplication(client, strings.NewReader(""), &output)
	if err := app.Run(context.Background(), []string{"git", "review", "submit", "--cwd", dir, "--yes", "--draft"}); err != nil {
		t.Fatal(err)
	}
	if !client.lastCreate.Draft {
		t.Fatal("Gitea draft intent was not passed to the adapter")
	}
	if !strings.Contains(output.String(), "Gitea PR") || !strings.Contains(output.String(), "#17") {
		t.Fatalf("missing Gitea label:\n%s", output.String())
	}
}

func (f *fakeReviewClient) Get(ctx context.Context, id string) (review.Review, error) {
	f.getCalls++
	if f.blockGet {
		<-ctx.Done()
		return review.Review{}, ctx.Err()
	}
	if len(f.getErrors) > 0 {
		err := f.getErrors[0]
		f.getErrors = f.getErrors[1:]
		return review.Review{}, err
	}
	if id != f.current.ID {
		return review.Review{}, fmt.Errorf("unknown review %s", id)
	}
	return f.current, nil
}

func TestPickSubmitFailureBeforePushRestoresOriginalCheckout(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "pending.txt"), []byte("pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "pending.txt")
	gitCommand(t, dir, "commit", "-m", "fix: pending")
	gitCommand(t, dir, "switch", "develop")
	originalHash := gitCommandOutput(t, dir, "rev-parse", "HEAD")

	client := &fakeReviewClient{findOpenErrors: []error{errors.New("injected pre-push lookup failure")}}
	var output bytes.Buffer
	app := reviewApplication(client, strings.NewReader(""), &output)
	app.Select = func(items []selector.Item, _ string) ([]selector.Item, error) { return items, nil }
	err := app.Run(context.Background(), []string{"pick", "fix/rollback", "--cwd", dir, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "failed before push") || !strings.Contains(err.Error(), "removed fix/rollback") {
		t.Fatalf("expected verified pre-push rollback, got %v", err)
	}
	if strings.Contains(output.String(), "✓ Branch") {
		t.Fatalf("rolled-back branch was reported as successful:\n%s", output.String())
	}
	if got := gitCommandOutput(t, dir, "branch", "--show-current"); got != "develop" {
		t.Fatalf("original checkout was not restored: %s", got)
	}
	if got := gitCommandOutput(t, dir, "rev-parse", "HEAD"); got != originalHash {
		t.Fatalf("original checkout hash changed: got %s want %s", got, originalHash)
	}
	assertBranchMissing(t, dir, "fix/rollback")
	if _, loadErr := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, "fix/rollback"); !errors.Is(loadErr, reviewstate.ErrNotFound) {
		t.Fatalf("picked review state remained after rollback: %v", loadErr)
	}
}

func TestPickSubmitProviderPreflightFailsBeforeSelectionOrBranchCreation(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "preflight.txt"), []byte("pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "preflight.txt")
	gitCommand(t, dir, "commit", "-m", "fix: preflight")
	gitCommand(t, dir, "switch", "develop")

	selected := false
	app := reviewApplication(&fakeReviewClient{}, strings.NewReader(""), io.Discard)
	app.ReviewClient = func(hosting.Repository) (review.Client, error) {
		return nil, errors.New("injected provider configuration failure")
	}
	app.Select = func(items []selector.Item, _ string) ([]selector.Item, error) {
		selected = true
		return items, nil
	}
	err := app.Run(context.Background(), []string{"pick", "fix/preflight", "--cwd", dir, "--yes", "--submit"})
	if err == nil || !strings.Contains(err.Error(), "initialize review provider before pick") {
		t.Fatalf("expected provider preflight failure, got %v", err)
	}
	if selected {
		t.Fatal("commit selection ran before provider preflight")
	}
	assertBranchMissing(t, dir, "fix/preflight")
}

func TestPickSubmitPushFailureKeepsBranchForUncertainRemoteRecovery(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "push-failure.txt"), []byte("pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "push-failure.txt")
	gitCommand(t, dir, "commit", "-m", "fix: preserve uncertain push")
	gitCommand(t, dir, "switch", "develop")

	app := reviewApplication(&fakeReviewClient{}, strings.NewReader(""), io.Discard)
	app.Git = func(dir string) gitservice.Service {
		return gitservice.Service{Dir: dir, Runner: failPushRunner{Runner: gitservice.ExecRunner{}}}
	}
	app.Select = func(items []selector.Item, _ string) ([]selector.Item, error) { return items, nil }
	err := app.Run(context.Background(), []string{"pick", "fix/push-uncertain", "--cwd", dir, "--yes", "--submit"})
	if err == nil || !strings.Contains(err.Error(), "after push started") || !strings.Contains(err.Error(), "kept for retry") {
		t.Fatalf("expected uncertain push preservation guidance, got %v", err)
	}
	assertBranchExists(t, dir, "fix/push-uncertain")
	state, loadErr := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, "fix/push-uncertain")
	if loadErr != nil || state.Stage != reviewstate.StagePicked {
		t.Fatalf("retryable picked state was not preserved: %#v, %v", state, loadErr)
	}
}

type failPushRunner struct {
	gitservice.Runner
}

func (r failPushRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "push" {
		return nil, errors.New("injected uncertain push failure")
	}
	return r.Runner.Run(ctx, dir, args...)
}

func (r failPushRunner) RunInput(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error) {
	return r.Runner.RunInput(ctx, dir, input, args...)
}

func TestReviewWaitRetriesTransientProviderTimeout(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	client, state, app := submitSimpleReview(t, dir, "fix/slow-review")
	client.getErrors = []error{fmt.Errorf("slow provider: %w", context.DeadlineExceeded)}
	client.current.Status = review.StatusMerged
	client.current.SourceSHA = state.PublishedTip
	mergedAt := time.Now().UTC()
	client.current.MergedAt = &mergedAt
	var output bytes.Buffer
	app.IO.Out = &output
	app.IO.ErrOut = &output

	err := app.reviewWaitWithService(context.Background(), globalOptions{}, reviewWaitOptions{branch: state.Branch, interval: time.Millisecond}, gitservice.Service{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if client.getCalls < 2 {
		t.Fatalf("transient timeout was not retried: %d calls", client.getCalls)
	}
	if !strings.Contains(output.String(), "Retry") || !strings.Contains(output.String(), "머지 완료") {
		t.Fatalf("retry and completion were not reported:\n%s", output.String())
	}
}

func TestReviewWaitDoesNotRetryNonTimeoutNetworkConfigurationError(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	client, state, app := submitSimpleReview(t, dir, "fix/config-error")
	client.getErrors = []error{&url.Error{Op: "Get", URL: "https://gitea.example", Err: errors.New("TLS configuration failed")}}
	err := app.reviewWaitWithService(context.Background(), globalOptions{}, reviewWaitOptions{branch: state.Branch, interval: time.Millisecond}, gitservice.Service{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "TLS configuration failed") {
		t.Fatalf("non-timeout configuration error was not returned: %v", err)
	}
	if client.getCalls != 1 {
		t.Fatalf("non-timeout configuration error was retried: %d calls", client.getCalls)
	}
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

func TestReviewSubmitAndStatusRefuseWorkflowSourceDrift(t *testing.T) {
	t.Run("submit", func(t *testing.T) {
		dir, _ := reviewRepositoryWithRemote(t)
		gitCommand(t, dir, "switch", "-c", "feat/submit-drift")
		if err := os.WriteFile(filepath.Join(dir, "submit-drift.txt"), []byte("drift\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCommand(t, dir, "add", "submit-drift.txt")
		gitCommand(t, dir, "commit", "-m", "feat: submit drift")
		service := gitservice.Service{Dir: dir}
		head := gitCommandOutput(t, dir, "rev-parse", "HEAD")
		state := reviewstate.State{Stage: reviewstate.StagePicked, Branch: "feat/submit-drift", SourceBranch: "work", TargetBranch: "develop", PickedTip: head}
		if err := reviewstate.Save(context.Background(), service, state); err != nil {
			t.Fatal(err)
		}
		gitCommand(t, dir, "config", "kit.git.source", "other-work")
		app := reviewApplication(&fakeReviewClient{}, strings.NewReader(""), io.Discard)
		err := app.Run(context.Background(), []string{"git", "review", "submit", "--cwd", dir, "--yes"})
		if err == nil || !strings.Contains(err.Error(), "workflow changed since pick") {
			t.Fatalf("expected submit source drift rejection, got %v", err)
		}
	})

	t.Run("status", func(t *testing.T) {
		dir, _ := reviewRepositoryWithRemote(t)
		_, state, app := submitSimpleReview(t, dir, "feat/status-drift")
		gitCommand(t, dir, "config", "kit.git.source", "other-work")
		err := app.Run(context.Background(), []string{"git", "review", "status", state.Branch, "--cwd", dir})
		if err == nil || !strings.Contains(err.Error(), "workflow changed since submit") {
			t.Fatalf("expected status source drift rejection, got %v", err)
		}
	})
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

func TestTopLevelSyncFinishesSingleMergedReview(t *testing.T) {
	dir, remote := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "automatic.txt"), []byte("automatic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "automatic.txt")
	gitCommand(t, dir, "commit", "-m", "feat: automatic finish")
	gitCommand(t, dir, "switch", "develop")

	client := &fakeReviewClient{}
	var output bytes.Buffer
	app := reviewApplication(client, strings.NewReader(""), &output)
	app.Select = func(items []selector.Item, _ string) ([]selector.Item, error) { return items, nil }
	if err := app.Run(context.Background(), []string{"pick", "feat/automatic", "--cwd", dir, "--yes"}); err != nil {
		t.Fatal(err)
	}
	state, err := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, "feat/automatic")
	if err != nil {
		t.Fatal(err)
	}
	client.current.Status = review.StatusMerged
	client.current.SourceSHA = state.PublishedTip
	client.current.MergeSHA = mergeReviewBranchNormally(t, remote, "feat/automatic")

	output.Reset()
	if err := app.Run(context.Background(), []string{"sync", "--cwd", dir, "--yes"}); err != nil {
		t.Fatal(err)
	}
	assertBranchMissing(t, dir, "feat/automatic")
	state, err = reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, "feat/automatic")
	if err != nil || state.Stage != reviewstate.StageCleaned {
		t.Fatalf("top-level sync did not finalize review: %#v, %v", state, err)
	}
	if !strings.Contains(output.String(), "kit · sync") || !strings.Contains(output.String(), "정리 완료") {
		t.Fatalf("top-level sync output is incomplete:\n%s", output.String())
	}
}

func TestTopLevelSyncDryRunPreviewsMergedReviewWithoutMutation(t *testing.T) {
	dir, remote := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "preview.txt"), []byte("preview\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "preview.txt")
	gitCommand(t, dir, "commit", "-m", "feat: preview finish")
	gitCommand(t, dir, "switch", "develop")

	client := &fakeReviewClient{}
	var output bytes.Buffer
	app := reviewApplication(client, strings.NewReader(""), &output)
	app.Select = func(items []selector.Item, _ string) ([]selector.Item, error) { return items, nil }
	if err := app.Run(context.Background(), []string{"pick", "feat/preview", "--cwd", dir, "--yes"}); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, "feat/preview")
	if err != nil {
		t.Fatal(err)
	}
	client.current.Status = review.StatusMerged
	client.current.SourceSHA = stateBefore.PublishedTip
	client.current.MergeSHA = mergeReviewBranchNormally(t, remote, "feat/preview")
	developBefore := gitCommandOutput(t, dir, "rev-parse", "develop")
	workBefore := gitCommandOutput(t, dir, "rev-parse", "work")

	output.Reset()
	if err := app.Run(context.Background(), []string{"sync", "--dry-run", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	stateAfter, err := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, "feat/preview")
	if err != nil {
		t.Fatal(err)
	}
	if stateAfter.Stage != stateBefore.Stage || !stateAfter.UpdatedAt.Equal(stateBefore.UpdatedAt) {
		t.Fatalf("dry-run persisted refreshed review state: before=%#v after=%#v", stateBefore, stateAfter)
	}
	assertBranchExists(t, dir, "feat/preview")
	if got := gitCommandOutput(t, dir, "rev-parse", "develop"); got != developBefore {
		t.Fatalf("dry-run changed develop: got %s want %s", got, developBefore)
	}
	if got := gitCommandOutput(t, dir, "rev-parse", "work"); got != workBefore {
		t.Fatalf("dry-run changed work: got %s want %s", got, workBefore)
	}
	if !strings.Contains(output.String(), "Mode") || !strings.Contains(output.String(), "Cleanup") {
		t.Fatalf("dry-run omitted merged review plan:\n%s", output.String())
	}
}

func TestTopLevelStatusRefreshesTrackedReview(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "branch", "work")
	client, state, app := submitSimpleReview(t, dir, "feat/status-refresh")
	client.current.Status = review.StatusMerged
	client.current.SourceSHA = state.PublishedTip
	client.current.MergeSHA = gitCommandOutput(t, dir, "rev-parse", "develop")
	var output bytes.Buffer
	app.IO.Out = &output
	app.IO.ErrOut = &output

	if err := app.Run(context.Background(), []string{"status", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	refreshed, err := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, state.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Stage != reviewstate.StageMerged {
		t.Fatalf("status did not persist provider review state: %#v", refreshed)
	}
	if !strings.Contains(output.String(), "머지 완료") || !strings.Contains(output.String(), "$ kit sync") {
		t.Fatalf("status did not show refreshed merge action:\n%s", output.String())
	}
}

func TestTopLevelStatusCachedDoesNotCallProvider(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "branch", "work")
	client, state, app := submitSimpleReview(t, dir, "feat/status-cached")
	client.current.Status = review.StatusMerged
	client.current.SourceSHA = state.PublishedTip
	client.current.MergeSHA = gitCommandOutput(t, dir, "rev-parse", "develop")
	var output bytes.Buffer
	app.IO.Out = &output
	app.IO.ErrOut = &output

	if err := app.Run(context.Background(), []string{"status", "--cached", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if client.getCalls != 0 {
		t.Fatalf("cached status called provider %d times", client.getCalls)
	}
	cached, err := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, state.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Stage != reviewstate.StageOpen {
		t.Fatalf("cached status mutated review state: %#v", cached)
	}
	if !strings.Contains(output.String(), "검토 중") {
		t.Fatalf("cached status did not show saved review state:\n%s", output.String())
	}
}

func TestTopLevelStatusTimeoutKeepsCachedReview(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "branch", "work")
	client, state, app := submitSimpleReview(t, dir, "feat/status-timeout")
	client.blockGet = true
	app.statusReviewRefreshTimeout = 10 * time.Millisecond
	var output bytes.Buffer
	app.IO.Out = &output
	app.IO.ErrOut = &output

	started := time.Now()
	if err := app.Run(context.Background(), []string{"status", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("status did not honor its provider refresh budget: %s", elapsed)
	}
	cached, err := reviewstate.Load(context.Background(), gitservice.Service{Dir: dir}, state.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Stage != reviewstate.StageOpen {
		t.Fatalf("timed out status mutated cached review: %#v", cached)
	}
	if !strings.Contains(output.String(), "Refresh") || !strings.Contains(output.String(), "cached 상태 표시") {
		t.Fatalf("timed out status omitted cached warning:\n%s", output.String())
	}
}

func TestTopLevelStatusWarnsWhenBaseIsAheadOfConfiguredRemote(t *testing.T) {
	dir, _ := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "remote", "rename", "origin", "upstream")
	gitCommand(t, dir, "config", "kit.git.remote", "upstream")
	gitCommand(t, dir, "branch", "work")
	if err := os.WriteFile(filepath.Join(dir, "local-base.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "local-base.txt")
	gitCommand(t, dir, "commit", "-m", "test: local base ahead")
	var output bytes.Buffer
	app := reviewApplication(&fakeReviewClient{}, strings.NewReader(""), &output)

	if err := app.Run(context.Background(), []string{"status", "--cached", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "develop · upstream보다 ahead 1 / behind 0") {
		t.Fatalf("status did not describe configured remote base drift:\n%s", output.String())
	}
	if strings.Contains(output.String(), "✓ Base") {
		t.Fatalf("status reported an ahead base as successful:\n%s", output.String())
	}
}

func TestDefaultPickSynchronizesRemoteBaseBeforeCreatingReview(t *testing.T) {
	dir, remote := reviewRepositoryWithRemote(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "pending-after-team.txt"), []byte("pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "pending-after-team.txt")
	gitCommand(t, dir, "commit", "-m", "feat: pending after team change")
	gitCommand(t, dir, "switch", "develop")

	teamClone := filepath.Join(t.TempDir(), "team")
	gitCommand(t, "", "clone", remote, teamClone)
	gitCommand(t, teamClone, "config", "user.name", "Kit Team Test")
	gitCommand(t, teamClone, "config", "user.email", "team@example.invalid")
	if err := os.WriteFile(filepath.Join(teamClone, "team.txt"), []byte("team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, teamClone, "add", "team.txt")
	gitCommand(t, teamClone, "commit", "-m", "feat: team change")
	gitCommand(t, teamClone, "push", "origin", "develop")

	app := reviewApplication(&fakeReviewClient{}, strings.NewReader(""), io.Discard)
	app.Select = func(items []selector.Item, _ string) ([]selector.Item, error) { return items, nil }
	if err := app.Run(context.Background(), []string{"pick", "feat/after-team", "--cwd", dir, "--yes"}); err != nil {
		t.Fatal(err)
	}
	if got := gitCommandOutput(t, dir, "show", "feat/after-team:team.txt"); got != "team" {
		t.Fatalf("review branch did not include the latest remote base: %q", got)
	}
	if got := gitCommandOutput(t, dir, "show", "feat/after-team:pending-after-team.txt"); got != "pending" {
		t.Fatalf("review branch did not include selected work: %q", got)
	}
}

func reviewRepositoryWithRemote(t *testing.T) (string, string) {
	t.Helper()
	dir := appRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, remote, "symbolic-ref", "HEAD", "refs/heads/develop")
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "config", "kit.git.provider", "gitea")
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

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"kit/internal/buildinfo"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
	"kit/internal/pickstate"
	"kit/internal/review"
	"kit/internal/reviewstate"
)

type getterReviewClient struct {
	item review.Review
}

func (c *getterReviewClient) Create(_ context.Context, request review.CreateRequest) (review.Review, error) {
	item := c.item
	if item.SourceBranch == "" {
		item.SourceBranch = request.SourceBranch
	}
	if item.TargetBranch == "" {
		item.TargetBranch = request.TargetBranch
	}
	return item, nil
}

func (c *getterReviewClient) Get(_ context.Context, _ int64) (review.Review, error) {
	return c.item, nil
}

func TestReviewListRefreshUpdatesSavedProviderState(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "remote", "add", "origin", "https://gitea.test/owner/repo.git")
	tip := gitCommandOutput(t, dir, "rev-parse", "develop")
	service := gitservice.Service{Dir: dir}
	if err := reviewstate.Save(context.Background(), service, reviewstate.State{
		Stage: reviewstate.StageOpen, Provider: "gitea", Remote: "origin",
		Branch: "feat/review", SourceBranch: "work", TargetBranch: "develop",
		ReviewID: "7", ReviewNumber: 7, ReviewURL: "https://gitea.test/owner/repo/pulls/7",
		Status: review.StatusOpen, PickedTip: tip, PublishedTip: tip,
	}); err != nil {
		t.Fatal(err)
	}
	client := &getterReviewClient{item: review.Review{
		Provider: "gitea", ID: "7", Number: 7, URL: "https://gitea.test/owner/repo/pulls/7",
		Status: review.StatusMerged, SourceBranch: "feat/review", TargetBranch: "develop",
		SourceSHA: tip, MergeSHA: tip,
	}}
	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &bytes.Buffer{}},
		Build: buildinfo.Current(),
		ReviewClient: func(hosting.Repository) (review.Client, error) {
			return client, nil
		},
	}
	if err := a.RunCLI(context.Background(), []string{"review", "list", "--refresh", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	var states []reviewstate.State
	if err := json.Unmarshal(output.Bytes(), &states); err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Stage != reviewstate.StageMerged || states[0].Status != review.StatusMerged {
		t.Fatalf("review state was not refreshed: %#v", states)
	}
}

func TestReviewFinishJSONReturnsFinalState(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "remote", "add", "origin", "https://gitea.test/owner/repo.git")
	tip := gitCommandOutput(t, dir, "rev-parse", "develop")
	now := time.Now().UTC()
	service := gitservice.Service{Dir: dir}
	if err := reviewstate.Save(context.Background(), service, reviewstate.State{
		Stage: reviewstate.StageCleaned, Provider: "gitea", Remote: "origin",
		Branch: "feat/done", SourceBranch: "work", TargetBranch: "develop",
		ReviewID: "8", ReviewNumber: 8, ReviewURL: "https://gitea.test/owner/repo/pulls/8",
		Status: review.StatusMerged, PickedTip: tip, PublishedTip: tip, MergeSHA: tip,
		SyncedAt: &now, CleanedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}
	client := &getterReviewClient{item: review.Review{
		Provider: "gitea", ID: "8", Number: 8, URL: "https://gitea.test/owner/repo/pulls/8",
		Status: review.StatusMerged, SourceBranch: "feat/done", TargetBranch: "develop",
		SourceSHA: tip, MergeSHA: tip,
	}}
	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &bytes.Buffer{}},
		Build: buildinfo.Current(),
		ReviewClient: func(hosting.Repository) (review.Client, error) {
			return client, nil
		},
	}
	if err := a.RunCLI(context.Background(), []string{"review", "finish", "feat/done", "--cwd", dir, "--yes", "--json"}); err != nil {
		t.Fatal(err)
	}
	var result reviewFinishJSONResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Finished || result.Review.Stage != reviewstate.StageCleaned || result.Review.ReviewNumber != 8 {
		t.Fatalf("unexpected finish JSON: %#v", result)
	}
}

func TestReviewFinishJSONRequiresYes(t *testing.T) {
	dir := appRepository(t)
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}, Build: buildinfo.Current()}
	err := a.RunCLI(context.Background(), []string{"review", "finish", "feat/done", "--cwd", dir, "--json"})
	if err == nil || !strings.Contains(err.Error(), "requires --yes") {
		t.Fatalf("expected --yes requirement, got %v", err)
	}
}

func TestDoctorRecoveryReportsCleanRepository(t *testing.T) {
	dir := appRepository(t)
	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &bytes.Buffer{}}, Build: buildinfo.Current()}
	if err := a.RunCLI(context.Background(), []string{"doctor", "--recovery", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	var result doctorResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("clean repository reported recovery problems: %#v", result.Checks)
	}
}

func TestDoctorRecoveryFindsInterruptedStateAndRefs(t *testing.T) {
	dir := appRepository(t)
	service := gitservice.Service{Dir: dir}
	ctx := context.Background()
	tip := gitCommandOutput(t, dir, "rev-parse", "develop")
	if err := pickstate.Save(ctx, service, pickstate.State{
		OriginalHash: tip, OriginalBranch: "develop", TargetBranch: "feat/paused",
		BaseBranch: "develop", SourceBranch: "work", Commits: []string{tip}, Next: 0,
	}); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "branch", "kit/recovery/test", tip)
	gitCommand(t, dir, "branch", "kit/tmp/test", tip)
	if err := service.MarkKitCreatedBranch(ctx, "feat/missing"); err != nil {
		t.Fatal(err)
	}
	// A retained backup is normal and must not make the recovery doctor fail by itself.
	gitCommand(t, dir, "branch", "kit/backup/test", tip)

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &bytes.Buffer{}}, Build: buildinfo.Current()}
	if err := a.RunCLI(ctx, []string{"doctor", "--recovery", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	var result doctorResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK {
		t.Fatalf("interrupted repository reported healthy: %#v", result.Checks)
	}
	for _, name := range []string{"pick state", "recovery refs", "temporary refs", "branch markers"} {
		if !doctorHasFailedCheck(result.Checks, name) {
			t.Fatalf("missing failed recovery check %q: %#v", name, result.Checks)
		}
	}
	if doctorHasFailedCheck(result.Checks, "backup refs") {
		t.Fatalf("normal backup refs must not fail recovery doctor: %#v", result.Checks)
	}
	_ = os.Remove(filepathJoinForTest(t, dir, ".never"))
}

func doctorHasFailedCheck(checks []doctorCheck, name string) bool {
	for _, check := range checks {
		if check.Name == name && !check.OK {
			return true
		}
	}
	return false
}

func filepathJoinForTest(t *testing.T, parts ...string) string {
	t.Helper()
	return strings.Join(parts, string(os.PathSeparator))
}

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	gitservice "kit/internal/git"
	"kit/internal/review"
	"kit/internal/reviewstate"
)

type recordingForkReviewClient struct {
	findOwner    string
	findSource   string
	findTarget   string
	createOwner  string
	create       review.CreateRequest
	existing     review.Review
	findExisting bool
	created      review.Review
}

func (c *recordingForkReviewClient) Create(_ context.Context, request review.CreateRequest) (review.Review, error) {
	return review.Review{}, errors.New("same-repository Create must not be used in fork mode")
}

func (c *recordingForkReviewClient) CreateFrom(_ context.Context, owner string, request review.CreateRequest) (review.Review, error) {
	c.createOwner = owner
	c.create = request
	return c.created, nil
}

func (c *recordingForkReviewClient) FindOpenFrom(_ context.Context, owner, source, target string) (review.Review, bool, error) {
	c.findOwner, c.findSource, c.findTarget = owner, source, target
	return c.existing, c.findExisting, nil
}

func TestResolveReviewRepositoriesForkTopology(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "remote", "add", "upstream", "https://gitea.example/upstream/repo.git")
	gitCommand(t, dir, "remote", "add", "origin", "https://gitea.example/forker/repo.git")
	config := gitservice.DefaultWorkflowConfig()
	config.Remote = "upstream"
	config.PushRemote = "origin"

	resolved, err := resolveReviewRepositories(context.Background(), gitservice.Service{Dir: dir}, config)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Fork || resolved.PushRemote != "origin" || resolved.SourceOwner != "forker" {
		t.Fatalf("unexpected fork resolution: %#v", resolved)
	}
	if resolved.Target.Owner != "upstream" || resolved.Target.Name != "repo" {
		t.Fatalf("unexpected target repository: %#v", resolved.Target)
	}
	if resolved.Source.Owner != "forker" || resolved.Source.Name != "repo" {
		t.Fatalf("unexpected source repository: %#v", resolved.Source)
	}
}

func TestResolveReviewRepositoriesRejectsCrossHostFork(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "remote", "add", "upstream", "https://gitea.example/upstream/repo.git")
	gitCommand(t, dir, "remote", "add", "origin", "https://other.example/forker/repo.git")
	config := gitservice.DefaultWorkflowConfig()
	config.Remote = "upstream"
	config.PushRemote = "origin"
	if _, err := resolveReviewRepositories(context.Background(), gitservice.Service{Dir: dir}, config); err == nil {
		t.Fatal("expected cross-host fork topology to be rejected")
	}
}

func TestFindOrCreateReviewUsesForkCapability(t *testing.T) {
	client := &recordingForkReviewClient{
		created: review.Review{Provider: "gitea", ID: "9", Number: 9, URL: "https://gitea.example/upstream/repo/pulls/9"},
	}
	repositories := reviewRepositories{Fork: true, SourceOwner: "forker"}
	created, reused, err := findOrCreateReview(context.Background(), client, repositories, "feat/login", "develop", "Login", "body", false, true)
	if err != nil || reused {
		t.Fatalf("unexpected fork create result: %#v reused=%v err=%v", created, reused, err)
	}
	if client.findOwner != "forker" || client.findSource != "feat/login" || client.findTarget != "develop" {
		t.Fatalf("fork lookup was not scoped correctly: %#v", client)
	}
	if client.createOwner != "forker" || client.create.SourceBranch != "feat/login" || client.create.TargetBranch != "develop" || !client.create.RemoveSourceBranch {
		t.Fatalf("fork create was not routed correctly: %#v", client)
	}
}

func TestFindOrCreateReviewReusesForkPR(t *testing.T) {
	existing := review.Review{Provider: "gitea", ID: "7", Number: 7, URL: "https://gitea.example/upstream/repo/pulls/7"}
	client := &recordingForkReviewClient{existing: existing, findExisting: true}
	got, reused, err := findOrCreateReview(context.Background(), client, reviewRepositories{Fork: true, SourceOwner: "forker"}, "feat/login", "develop", "Login", "", false, true)
	if err != nil || !reused || got.Number != 7 {
		t.Fatalf("unexpected fork reuse result: %#v reused=%v err=%v", got, reused, err)
	}
	if client.createOwner != "" {
		t.Fatalf("CreateFrom was called despite existing PR: %#v", client)
	}
}

func TestCleanupFinishedForkReviewDeletesOnlyPushRemoteBranch(t *testing.T) {
	dir := appRepository(t)
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	fork := filepath.Join(t.TempDir(), "fork.git")
	gitCommand(t, "", "init", "--bare", upstream)
	gitCommand(t, "", "init", "--bare", fork)
	gitCommand(t, dir, "remote", "add", "upstream", upstream)
	gitCommand(t, dir, "remote", "add", "origin", fork)
	gitCommand(t, dir, "branch", "work", "develop")
	gitCommand(t, dir, "switch", "-c", "feat/fork")
	if err := os.WriteFile(filepath.Join(dir, "fork.txt"), []byte("fork\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "fork.txt")
	gitCommand(t, dir, "commit", "-m", "fork review")
	tip := gitCommandOutput(t, dir, "rev-parse", "HEAD")
	gitCommand(t, dir, "push", "-u", "origin", "feat/fork")
	gitCommand(t, dir, "push", "upstream", "feat/fork")
	gitCommand(t, dir, "switch", "develop")
	service := gitservice.Service{Dir: dir}
	if err := service.MarkKitCreatedBranch(context.Background(), "feat/fork"); err != nil {
		t.Fatal(err)
	}
	config := gitservice.DefaultWorkflowConfig()
	config.Remote = "upstream"
	config.PushRemote = "origin"
	state := reviewstate.State{Remote: "origin", Branch: "feat/fork", PublishedTip: tip}
	localRemoved, remoteRemoved, err := cleanupFinishedReviewBranch(context.Background(), service, config, state, true)
	if err != nil {
		t.Fatal(err)
	}
	if !localRemoved || !remoteRemoved {
		t.Fatalf("expected local+fork cleanup, got local=%v remote=%v", localRemoved, remoteRemoved)
	}
	if commandSucceeds(fork, "show-ref", "--verify", "--quiet", "refs/heads/feat/fork") {
		t.Fatal("fork review branch remained after cleanup")
	}
	if !commandSucceeds(upstream, "show-ref", "--verify", "--quiet", "refs/heads/feat/fork") {
		t.Fatal("upstream branch with same name was incorrectly deleted")
	}
}

func TestCleanupFinishedForkReviewRejectsRemoteChangeBeforeLocalDeletion(t *testing.T) {
	dir := appRepository(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	other := filepath.Join(t.TempDir(), "other.git")
	gitCommand(t, "", "init", "--bare", origin)
	gitCommand(t, "", "init", "--bare", other)
	gitCommand(t, dir, "remote", "add", "origin", origin)
	gitCommand(t, dir, "remote", "add", "other", other)
	gitCommand(t, dir, "branch", "work", "develop")
	gitCommand(t, dir, "branch", "feat/saved", "develop")
	service := gitservice.Service{Dir: dir}
	if err := service.MarkKitCreatedBranch(context.Background(), "feat/saved"); err != nil {
		t.Fatal(err)
	}
	tip := gitCommandOutput(t, dir, "rev-parse", "feat/saved")
	config := gitservice.DefaultWorkflowConfig()
	config.PushRemote = "other"
	state := reviewstate.State{Remote: "origin", Branch: "feat/saved", PublishedTip: tip}
	localRemoved, remoteRemoved, err := cleanupFinishedReviewBranch(context.Background(), service, config, state, true)
	if err == nil {
		t.Fatal("expected saved/config push remote mismatch")
	}
	if localRemoved || remoteRemoved {
		t.Fatalf("cleanup mutated before rejecting mismatch: local=%v remote=%v", localRemoved, remoteRemoved)
	}
	if !commandSucceeds(dir, "show-ref", "--verify", "--quiet", "refs/heads/feat/saved") {
		t.Fatal("local review branch was deleted before push remote mismatch rejection")
	}
}

var _ review.Client = (*recordingForkReviewClient)(nil)
var _ review.ForkClient = (*recordingForkReviewClient)(nil)

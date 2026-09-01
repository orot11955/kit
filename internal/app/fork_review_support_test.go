package app

import (
	"context"
	"errors"
	"testing"

	gitservice "kit/internal/git"
	"kit/internal/review"
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

var _ review.Client = (*recordingForkReviewClient)(nil)
var _ review.ForkClient = (*recordingForkReviewClient)(nil)

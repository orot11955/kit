package git

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCherryPickedFromReturnsXTrailerSource(t *testing.T) {
	dir := initRepository(t)
	gitRun(t, dir, "switch", "-c", "work")
	writeAndCommit(t, dir, "a.txt", "a\n", "add a")
	original := gitOutput(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "switch", "develop")
	gitRun(t, dir, "cherry-pick", "-x", original)
	picked := gitOutput(t, dir, "rev-parse", "HEAD")

	got, ok, err := (Service{Dir: dir}).CherryPickedFrom(context.Background(), picked)
	if err != nil || !ok || got != original {
		t.Fatalf("CherryPickedFrom=%q ok=%v err=%v, want %s", got, ok, err, original)
	}
}

func TestDeleteRemoteBranchIfMatchesRequiresExactTip(t *testing.T) {
	dir := initRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, "", "init", "--bare", remote)
	gitRun(t, dir, "remote", "add", "origin", remote)
	gitRun(t, dir, "push", "origin", "develop")
	gitRun(t, dir, "switch", "-c", "feat/review")
	writeAndCommit(t, dir, "review.txt", "review\n", "review")
	tip := gitOutput(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "push", "origin", "feat/review")

	service := Service{Dir: dir}
	wrong := strings.Repeat("0", 40)
	if _, err := service.DeleteRemoteBranchIfMatches(context.Background(), "origin", "feat/review", wrong); err == nil || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("expected mismatched-tip refusal, got %v", err)
	}
	if _, exists, err := service.RemoteBranchHash(context.Background(), "origin", "feat/review"); err != nil || !exists {
		t.Fatalf("remote branch disappeared after mismatch: exists=%v err=%v", exists, err)
	}
	deleted, err := service.DeleteRemoteBranchIfMatches(context.Background(), "origin", "feat/review", tip)
	if err != nil || !deleted {
		t.Fatalf("expected exact-tip deletion, deleted=%v err=%v", deleted, err)
	}
	if _, exists, err := service.RemoteBranchHash(context.Background(), "origin", "feat/review"); err != nil || exists {
		t.Fatalf("remote branch still exists after deletion: exists=%v err=%v", exists, err)
	}
}

package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWorktreeLifecycle(t *testing.T) {
	dir := initRepository(t)
	service := Service{Dir: dir}
	root, err := service.TopLevel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sameExistingPath(t, root, dir) {
		t.Fatalf("top level=%q want same filesystem path as %q", root, filepath.Clean(dir))
	}

	createdPath := filepath.Join(t.TempDir(), "created")
	if err := service.AddWorktreeBranch(context.Background(), createdPath, "feat/worktree", "develop"); err != nil {
		t.Fatal(err)
	}
	worktrees, err := service.Worktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range worktrees {
		if sameExistingPath(t, item.Path, createdPath) {
			found = true
			if item.Branch != "feat/worktree" || item.Detached || item.Head == "" {
				t.Fatalf("unexpected worktree: %#v", item)
			}
		}
	}
	if !found {
		t.Fatalf("created worktree not listed: %#v", worktrees)
	}

	if err := service.RemoveWorktree(context.Background(), createdPath, false); err != nil {
		t.Fatal(err)
	}
	if err := service.PruneWorktrees(context.Background()); err != nil {
		t.Fatal(err)
	}

	existingPath := filepath.Join(t.TempDir(), "existing")
	if err := service.AddWorktree(context.Background(), existingPath, "feat/worktree"); err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveWorktree(context.Background(), existingPath, false); err != nil {
		t.Fatal(err)
	}
}

func sameExistingPath(t *testing.T, left, right string) bool {
	t.Helper()
	leftInfo, err := os.Stat(left)
	if err != nil {
		t.Fatalf("stat %s: %v", left, err)
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		t.Fatalf("stat %s: %v", right, err)
	}
	return os.SameFile(leftInfo, rightInfo)
}

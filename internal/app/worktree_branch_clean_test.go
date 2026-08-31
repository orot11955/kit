package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kit/internal/buildinfo"
	gitservice "kit/internal/git"
)

func TestWorktreeCommandCreateListAndRemoveKeepsBranch(t *testing.T) {
	dir := appRepository(t)
	path := filepath.Join(t.TempDir(), "linked")
	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}

	if err := a.RunCLI(context.Background(), []string{"worktree", "add", "feat/linked", path, "--create", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	if !commandSucceeds(dir, "show-ref", "--verify", "--quiet", "refs/heads/feat/linked") {
		t.Fatal("created branch is missing")
	}
	output.Reset()
	if err := a.RunCLI(context.Background(), []string{"worktree", "list", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	var listed []gitservice.Worktree
	if err := json.Unmarshal(output.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range listed {
		if filepath.Clean(item.Path) == filepath.Clean(path) && item.Branch == "feat/linked" {
			found = true
		}
	}
	if !found {
		t.Fatalf("linked worktree not found: %#v", listed)
	}

	output.Reset()
	if err := a.RunCLI(context.Background(), []string{"worktree", "remove", path, "--cwd", dir, "--yes", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !commandSucceeds(dir, "show-ref", "--verify", "--quiet", "refs/heads/feat/linked") {
		t.Fatal("worktree remove must keep branch")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree path still exists: %v", err)
	}
}

func TestBranchCleanClassifiesAndDeletesOnlySafeKitBranches(t *testing.T) {
	dir := appRepository(t)
	service := gitservice.Service{Dir: dir}
	ctx := context.Background()

	gitCommand(t, dir, "switch", "-c", "feat/applied")
	if err := os.WriteFile(filepath.Join(dir, "applied.txt"), []byte("applied\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "applied.txt")
	gitCommand(t, dir, "commit", "-m", "applied change")
	appliedOriginal := gitCommandOutput(t, dir, "rev-parse", "HEAD")
	if err := service.MarkKitCreatedBranch(ctx, "feat/applied"); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "switch", "develop")
	gitCommand(t, dir, "cherry-pick", "-x", appliedOriginal)

	gitCommand(t, dir, "switch", "-c", "feat/pending")
	if err := os.WriteFile(filepath.Join(dir, "pending.txt"), []byte("pending\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "pending.txt")
	gitCommand(t, dir, "commit", "-m", "pending change")
	if err := service.MarkKitCreatedBranch(ctx, "feat/pending"); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "switch", "develop")

	gitCommand(t, dir, "branch", "feat/in-use", "develop")
	if err := service.MarkKitCreatedBranch(ctx, "feat/in-use"); err != nil {
		t.Fatal(err)
	}
	worktreePath := filepath.Join(t.TempDir(), "in-use")
	gitCommand(t, dir, "worktree", "add", worktreePath, "feat/in-use")
	t.Cleanup(func() { _ = service.RemoveWorktree(context.Background(), worktreePath, true) })

	if err := service.MarkKitCreatedBranch(ctx, "feat/missing"); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.RunCLI(ctx, []string{"branch-clean", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	var dry branchCleanResult
	if err := json.Unmarshal(output.Bytes(), &dry); err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || !containsBranchCleanEntry(dry.Candidates, "feat/applied") {
		t.Fatalf("safe applied branch was not a dry-run candidate: %#v", dry)
	}
	if !containsBranchCleanEntry(dry.Protected, "feat/in-use") {
		t.Fatalf("worktree branch was not protected: %#v", dry)
	}
	if !containsBranchCleanEntry(dry.Retained, "feat/pending") {
		t.Fatalf("pending branch was not retained: %#v", dry)
	}
	if !containsString(dry.ClearedMarks, "feat/missing") {
		t.Fatalf("stale marker was not reported: %#v", dry)
	}

	output.Reset()
	if err := a.RunCLI(ctx, []string{"branch-clean", "--delete", "--yes", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	if commandSucceeds(dir, "show-ref", "--verify", "--quiet", "refs/heads/feat/applied") {
		t.Fatal("safe applied branch was not deleted")
	}
	for _, branch := range []string{"feat/pending", "feat/in-use"} {
		if !commandSucceeds(dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch) {
			t.Fatalf("protected/retained branch %s was deleted", branch)
		}
	}
	if marked, err := service.IsKitCreatedBranch(ctx, "feat/applied"); err != nil || marked {
		t.Fatalf("deleted branch marker remained: marked=%v err=%v", marked, err)
	}
	if marked, err := service.IsKitCreatedBranch(ctx, "feat/missing"); err != nil || marked {
		t.Fatalf("stale marker remained: marked=%v err=%v", marked, err)
	}
}

func containsBranchCleanEntry(entries []branchCleanEntry, branch string) bool {
	for _, entry := range entries {
		if entry.Branch == branch {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

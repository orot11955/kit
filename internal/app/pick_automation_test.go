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

func TestSelectCommitsByRefsPreservesPendingOrder(t *testing.T) {
	pending := []gitservice.Commit{
		{Hash: strings.Repeat("a", 40), ShortHash: "aaaaaaaa"},
		{Hash: strings.Repeat("b", 40), ShortHash: "bbbbbbbb"},
		{Hash: strings.Repeat("c", 40), ShortHash: "cccccccc"},
	}
	selected, err := selectCommitsByRefs(pending, []string{"cccccccc", "aaaaaaaa"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0].Hash != pending[0].Hash || selected[1].Hash != pending[2].Hash {
		t.Fatalf("selection did not preserve pending order: %#v", selected)
	}
}

func TestPickDryRunJSONDoesNotCreateBranchAndPreservesOrder(t *testing.T) {
	dir, _, first, second := setupAutomatedPickRepository(t)
	originalHead := gitCommandOutput(t, dir, "rev-parse", "HEAD")
	var output bytes.Buffer
	app := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Current(),
	}
	if err := app.Run(context.Background(), []string{
		"pick", "feat/dry", "--commit", second[:8], "--commit", first[:8],
		"--dry-run", "--json", "--cwd", dir,
	}); err != nil {
		t.Fatal(err)
	}
	var plan automatedPickPlan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatalf("decode dry-run JSON: %v\n%s", err, output.String())
	}
	if !plan.DryRun || plan.Target != "feat/dry" || len(plan.Commits) != 2 {
		t.Fatalf("unexpected dry-run plan: %#v", plan)
	}
	if plan.Commits[0].Hash != first || plan.Commits[1].Hash != second {
		t.Fatalf("dry-run selection order=%#v, want %s then %s", plan.Commits, first, second)
	}
	assertBranchMissing(t, dir, "feat/dry")
	if branch := gitCommandOutput(t, dir, "branch", "--show-current"); branch != "develop" {
		t.Fatalf("dry-run changed checkout: %s", branch)
	}
	if head := gitCommandOutput(t, dir, "rev-parse", "HEAD"); head != originalHead {
		t.Fatalf("dry-run changed HEAD: got %s want %s", head, originalHead)
	}
}

func TestPickCommitLocalJSONCreatesSelectedCommitsInPendingOrder(t *testing.T) {
	dir, _, first, second := setupAutomatedPickRepository(t)
	var output bytes.Buffer
	app := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Current(),
	}
	if err := app.Run(context.Background(), []string{
		"pick", "feat/local", "--commit", second[:8], "--commit", first[:8],
		"--local", "--yes", "--json", "--cwd", dir,
	}); err != nil {
		t.Fatal(err)
	}
	var result automatedLocalPickResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode local pick JSON: %v\n%s", err, output.String())
	}
	if result.Branch != "feat/local" || len(result.Commits) != 2 {
		t.Fatalf("unexpected local pick result: %#v", result)
	}
	if result.Commits[0] != first || result.Commits[1] != second {
		t.Fatalf("result commit order=%#v, want [%s %s]", result.Commits, first, second)
	}
	if branch := gitCommandOutput(t, dir, "branch", "--show-current"); branch != "feat/local" {
		t.Fatalf("expected created branch checkout, got %s", branch)
	}
	messages := strings.Split(gitCommandOutput(t, dir, "log", "--reverse", "--format=%s", "develop..feat/local"), "\n")
	if len(messages) != 2 || messages[0] != "first work" || messages[1] != "second work" {
		t.Fatalf("unexpected cherry-pick order: %#v", messages)
	}
}

func setupAutomatedPickRepository(t *testing.T) (dir, remote, first, second string) {
	t.Helper()
	dir = appRepository(t)
	remote = filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "push", "-u", "origin", "develop")
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "first.txt")
	gitCommand(t, dir, "commit", "-m", "first work")
	first = gitCommandOutput(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "second.txt")
	gitCommand(t, dir, "commit", "-m", "second work")
	second = gitCommandOutput(t, dir, "rev-parse", "HEAD")
	gitCommand(t, dir, "switch", "develop")
	return dir, remote, first, second
}

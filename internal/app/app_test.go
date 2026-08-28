package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kit/internal/buildinfo"
	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/review"
	"kit/internal/selector"
)

type createOnlyReviewClient struct{}

func (createOnlyReviewClient) Create(context.Context, review.CreateRequest) (review.Review, error) {
	return review.Review{}, nil
}

var _ review.Client = createOnlyReviewClient{}

func TestParseCompareFlagsInAnyOrder(t *testing.T) {
	opts, help, err := parseCompare(globalOptions{cwd: "."}, []string{"work-2", "--limit", "4", "--base=main", "--json", "--cwd", "/tmp/repo"})
	if err != nil || help {
		t.Fatalf("parseCompare: %v, help=%v", err, help)
	}
	if opts.source != "work-2" || opts.base != "main" || opts.limit != 4 || !opts.json || opts.cwd != "/tmp/repo" {
		t.Fatalf("unexpected options: %#v", opts)
	}
}

func TestParsePickRequiresOneTarget(t *testing.T) {
	_, _, err := parsePick(globalOptions{cwd: "."}, []string{"--from", "work"})
	if clierror.Code(err) != clierror.Usage {
		t.Fatalf("expected usage error, got %v", err)
	}
}

func TestParsePickSubmitsByDefaultAndLocalOptOut(t *testing.T) {
	defaultOptions, _, err := parsePick(globalOptions{cwd: "."}, []string{"feat/default"})
	if err != nil {
		t.Fatal(err)
	}
	if !defaultOptions.submit || defaultOptions.localOnly {
		t.Fatalf("pick should submit by default: %#v", defaultOptions)
	}
	if !defaultOptions.submit {
		t.Fatalf("default pick should submit and return after Create: %#v", defaultOptions)
	}
	noWaitOptions, _, err := parsePick(globalOptions{cwd: "."}, []string{"feat/no-wait", "--no-wait"})
	if err != nil || !noWaitOptions.submit {
		t.Fatalf("--no-wait should be accepted as a deprecated Create-only compatibility option: %#v, %v", noWaitOptions, err)
	}
	localOptions, _, err := parsePick(globalOptions{cwd: "."}, []string{"feat/local", "--local"})
	if err != nil {
		t.Fatal(err)
	}
	if localOptions.submit || !localOptions.localOnly {
		t.Fatalf("--local should disable submit: %#v", localOptions)
	}
	allOptions, _, err := parsePick(globalOptions{cwd: "."}, []string{"feat/all", "--all", "--local"})
	if err != nil {
		t.Fatal(err)
	}
	if !allOptions.all {
		t.Fatalf("--all was not parsed: %#v", allOptions)
	}
}

func TestCompareListsFirstParentCommitsFromMergedWork(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	gitCommand(t, dir, "switch", "-c", "side")
	if err := os.WriteFile(filepath.Join(dir, "side.txt"), []byte("side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "side.txt")
	gitCommand(t, dir, "commit", "-m", "side")
	gitCommand(t, dir, "switch", "work")
	gitCommand(t, dir, "merge", "--no-ff", "side", "-m", "merge side")

	a := &Application{IO: IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"compare", "--cwd", dir}); err != nil {
		t.Fatalf("compare rejected merged work: %v", err)
	}
}

func TestPickAllowsMergedBaseAlreadyContainedByWork(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "base.txt")
	gitCommand(t, dir, "commit", "-m", "base")
	gitCommand(t, dir, "switch", "work")
	workBefore := gitCommandOutput(t, dir, "rev-parse", "HEAD")
	gitCommand(t, dir, "merge", "--no-ff", "develop", "-m", "merge base")
	workAfter := gitCommandOutput(t, dir, "rev-parse", "HEAD")
	a := &Application{IO: IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"pick", "feat/merged-base", "--all", "--local", "--yes", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if got := gitCommandOutput(t, dir, "rev-parse", "work"); got != workAfter {
		t.Fatalf("work changed: got %s want %s (pre-merge %s)", got, workAfter, workBefore)
	}
	if got := gitCommandOutput(t, dir, "show", "feat/merged-base:feature.txt"); got != "feature" {
		t.Fatalf("target omitted pending feature: %q", got)
	}
	if got := gitCommandOutput(t, dir, "show", "feat/merged-base:base.txt"); got != "base" {
		t.Fatalf("target was not based on develop: %q", got)
	}
}

func TestPickSkipsSideMergeWithoutPreflightRejection(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	gitCommand(t, dir, "switch", "-c", "side")
	if err := os.WriteFile(filepath.Join(dir, "side.txt"), []byte("side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "side.txt")
	gitCommand(t, dir, "commit", "-m", "side")
	gitCommand(t, dir, "switch", "work")
	gitCommand(t, dir, "merge", "--no-ff", "side", "-m", "merge side")
	a := &Application{IO: IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"pick", "feat/reject-side", "--all", "--local", "--yes", "--cwd", dir}); err != nil {
		t.Fatalf("pick rejected side merge: %v", err)
	}
	assertBranchMissing(t, dir, "feat/reject-side")
	if commandSucceeds(dir, "rev-parse", "--verify", "HEAD:kit/pick-state.json") {
		t.Fatal("pickstate was created before preflight rejection")
	}
}

func TestLegacyGitPickDefaultsToLocalOnly(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "legacy.txt"), []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "legacy.txt")
	gitCommand(t, dir, "commit", "-m", "legacy local pick")
	gitCommand(t, dir, "switch", "develop")

	var output bytes.Buffer
	a := &Application{
		IO:     IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build:  buildinfo.Current(),
		Select: func(items []selector.Item, _ string) ([]selector.Item, error) { return items, nil },
	}
	if err := a.Run(context.Background(), []string{"git", "pick", "feat/legacy", "--cwd", dir, "--yes"}); err != nil {
		t.Fatal(err)
	}
	assertBranchExists(t, dir, "feat/legacy")
	if !strings.Contains(output.String(), "호환 명령") || !strings.Contains(output.String(), "로컬 브랜치 생성") {
		t.Fatalf("legacy pick did not explain local-only behavior:\n%s", output.String())
	}
}

func TestDefaultPickRejectsCustomWorkflowWithoutLocal(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "branch", "work")
	selected := false
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard},
		Build: buildinfo.Current(),
		Select: func(items []selector.Item, _ string) ([]selector.Item, error) {
			selected = true
			return items, nil
		},
	}
	err := a.Run(context.Background(), []string{"pick", "feat/custom", "--from", "other-work", "--cwd", dir})
	if clierror.Code(err) != clierror.Usage || !strings.Contains(err.Error(), "--local") {
		t.Fatalf("expected early custom workflow guidance, got %v", err)
	}
	if selected {
		t.Fatal("selector opened before rejecting unsupported custom submit workflow")
	}
}

func TestVersionJSON(t *testing.T) {
	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Info{Version: "v1.2.3", Commit: "abc", BuildDate: "date", Target: "darwin/arm64"},
	}
	if err := a.Run(context.Background(), []string{"version", "--json"}); err != nil {
		t.Fatal(err)
	}
	var got buildinfo.Info
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != a.Build {
		t.Fatalf("got %#v, want %#v", got, a.Build)
	}
}

func TestTopLevelStatusAndBackupCommandsUseCanonicalUI(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "branch", "work")
	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"status", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"kit · status", "저장소", "워크플로", "리뷰"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("status UI omitted %q:\n%s", expected, output.String())
		}
	}
	output.Reset()
	if err := a.Run(context.Background(), []string{"backup", "list", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "kit · backup list") {
		t.Fatalf("backup alias did not use canonical UI:\n%s", output.String())
	}
}

func TestCompareJSONClassifiesCommits(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"compare", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	var got compareResult
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Pending != 1 || got.Applied != 0 || len(got.Commits) != 1 {
		t.Fatalf("unexpected compare result: %#v", got)
	}
}

func TestPickReportsNonTTYBeforeMutation(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")
	gitCommand(t, dir, "switch", "develop")

	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Current(),
		Select: func([]selector.Item, string) ([]selector.Item, error) {
			return nil, selector.ErrNotTTY
		},
	}
	err := a.Run(context.Background(), []string{"pick", "feat/test", "--local", "--cwd", dir})
	if clierror.Code(err) != clierror.Failure || !strings.Contains(err.Error(), "TTY") {
		t.Fatalf("expected non-TTY error, got %v", err)
	}
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/feat/test")
	cmd.Dir = dir
	if runErr := cmd.Run(); runErr == nil {
		t.Fatal("target branch was created before TTY validation")
	}
}

func TestPickRejectsInvalidTargetBeforeSelection(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")
	gitCommand(t, dir, "switch", "develop")

	selected := false
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard},
		Build: buildinfo.Current(),
		Select: func([]selector.Item, string) ([]selector.Item, error) {
			selected = true
			return nil, nil
		},
	}
	err := a.Run(context.Background(), []string{"pick", "bad..branch", "--cwd", dir})
	if clierror.Code(err) != clierror.Failure || !strings.Contains(err.Error(), "invalid target branch") {
		t.Fatalf("expected invalid branch error, got %v", err)
	}
	if selected {
		t.Fatal("selector opened for an invalid branch")
	}
}

func TestPickCreatesBranchAndAppliesSourceOrder(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "first feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "second feature")
	gitCommand(t, dir, "switch", "develop")

	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Current(),
		Select: func(items []selector.Item, _ string) ([]selector.Item, error) {
			return items, nil
		},
	}
	if err := a.Run(context.Background(), []string{"pick", "feat/test", "--local", "--cwd", dir, "--yes"}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "log", "--reverse", "--format=%s", "develop..feat/test")
	cmd.Dir = dir
	log, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(log)); got != "first feature\nsecond feature" {
		t.Fatalf("commits were not applied oldest-first:\n%s", got)
	}
	messageCmd := exec.Command("git", "log", "-1", "--format=%B", "feat/test")
	messageCmd.Dir = dir
	message, err := messageCmd.Output()
	if err != nil || !strings.Contains(string(message), "cherry picked from commit") {
		t.Fatalf("missing -x record: %v\n%s", err, message)
	}
}

func TestPickAllSelectsEveryPendingCommitWithoutSelector(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "first.txt")
	gitCommand(t, dir, "commit", "-m", "first pending commit")
	if err := os.WriteFile(filepath.Join(dir, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "second.txt")
	gitCommand(t, dir, "commit", "-m", "second pending commit")
	gitCommand(t, dir, "switch", "develop")

	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Current(),
		Select: func([]selector.Item, string) ([]selector.Item, error) {
			t.Fatal("--all opened the interactive selector")
			return nil, nil
		},
	}
	if err := a.Run(context.Background(), []string{"pick", "chore/all", "--all", "--local", "--cwd", dir, "--yes"}); err != nil {
		t.Fatal(err)
	}
	log := gitCommandOutput(t, dir, "log", "--reverse", "--format=%s", "develop..chore/all")
	if log != "first pending commit\nsecond pending commit" {
		t.Fatalf("--all did not preserve every pending commit in source order:\n%s", log)
	}
	if !strings.Contains(output.String(), "선택한 커밋 · 2개") {
		t.Fatalf("--all plan omitted its selected count:\n%s", output.String())
	}
}

func TestPickRejectsDirtyTreeBeforeSelection(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	selected := false
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard},
		Build: buildinfo.Current(),
		Select: func([]selector.Item, string) ([]selector.Item, error) {
			selected = true
			return nil, nil
		},
	}
	err := a.Run(context.Background(), []string{"pick", "feat/dirty", "--local", "--cwd", dir})
	if clierror.Code(err) != clierror.Failure || !strings.Contains(err.Error(), "working tree has changes") {
		t.Fatalf("expected dirty-tree error, got %v", err)
	}
	if selected {
		t.Fatal("selector opened for a dirty working tree")
	}
	assertBranchMissing(t, dir, "feat/dirty")
}

func TestPickConflictAbortRestoresCheckoutAndDeletesTarget(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("work change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "work change")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("develop change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "develop change")
	originalHead := gitCommandOutput(t, dir, "rev-parse", "HEAD")

	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader("a\n"), Out: &output, ErrOut: &output},
		Build: buildinfo.Current(),
		Select: func(items []selector.Item, _ string) ([]selector.Item, error) {
			return items, nil
		},
	}
	err := a.Run(context.Background(), []string{"pick", "feat/conflict", "--local", "--cwd", dir, "--yes", "--allow-stale"})
	if clierror.Code(err) != clierror.Failure || !strings.Contains(err.Error(), "restored the original checkout") {
		t.Fatalf("expected clean abort result, got %v\n%s", err, output.String())
	}
	if branch := gitCommandOutput(t, dir, "branch", "--show-current"); branch != "develop" {
		t.Fatalf("checkout was not restored: %s", branch)
	}
	if head := gitCommandOutput(t, dir, "rev-parse", "HEAD"); head != originalHead {
		t.Fatalf("HEAD changed after abort: got %s want %s", head, originalHead)
	}
	assertBranchMissing(t, dir, "feat/conflict")
	if marked, markerErr := (gitservice.Service{Dir: dir}).IsKitCreatedBranch(context.Background(), "feat/conflict"); markerErr != nil || marked {
		t.Fatalf("Kit branch marker remained after pick abort: marked=%v err=%v", marked, markerErr)
	}
	if status := gitCommandOutput(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("working tree is dirty after abort: %s", status)
	}
	content, readErr := os.ReadFile(filepath.Join(dir, "root.txt"))
	if readErr != nil || string(content) != "develop change\n" {
		t.Fatalf("working file was not restored: %q, %v", content, readErr)
	}
}

func TestPickCanResumeAfterProcessExit(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("work change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "work change")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("develop change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "develop change")

	a := &Application{
		IO:    IO{In: strings.NewReader("q\n"), Out: io.Discard, ErrOut: io.Discard},
		Build: buildinfo.Current(),
		Select: func(items []selector.Item, _ string) ([]selector.Item, error) {
			return items, nil
		},
	}
	err := a.Run(context.Background(), []string{"pick", "feat/resume", "--local", "--cwd", dir, "--yes", "--allow-stale"})
	if clierror.Code(err) != clierror.Conflict || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("expected paused pick, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")

	resume := &Application{IO: IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard}, Build: buildinfo.Current()}
	if err := resume.Run(context.Background(), []string{"pick", "--continue", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if branch := gitCommandOutput(t, dir, "branch", "--show-current"); branch != "feat/resume" {
		t.Fatalf("unexpected branch after resume: %s", branch)
	}
	statePath := filepath.Join(dir, ".git", "kit", "pick-state.json")
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pick state was not removed: %v", err)
	}
}

func TestPickContinueAcceptsConflictResolutionCommittedInIDE(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("work change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "work change")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("develop change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "develop change")

	pause := &Application{
		IO:    IO{In: strings.NewReader("q\n"), Out: io.Discard, ErrOut: io.Discard},
		Build: buildinfo.Current(),
		Select: func(items []selector.Item, _ string) ([]selector.Item, error) {
			return items, nil
		},
	}
	err := pause.Run(context.Background(), []string{"pick", "feat/ide", "--local", "--cwd", dir, "--yes", "--allow-stale"})
	if clierror.Code(err) != clierror.Conflict {
		t.Fatalf("expected paused conflict, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("resolved in IDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "resolve conflict in IDE")

	resume := &Application{IO: IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard}, Build: buildinfo.Current()}
	if err := resume.Run(context.Background(), []string{"pick", "--continue", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if got := gitCommandOutput(t, dir, "show", "feat/ide:root.txt"); got != "resolved in IDE" {
		t.Fatalf("external resolution commit was not kept: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "kit", "pick-state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pick state was not finalized: %v", err)
	}
}

func TestCanceledSelectorIsSuccess(t *testing.T) {
	if !errors.Is(selector.ErrCanceled, selector.ErrCanceled) {
		t.Fatal("sentinel broken")
	}
}

func TestGitNamespaceCompareUsesRepositoryWorkflowConfig(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "branch", "-m", "integration")
	gitCommand(t, dir, "switch", "-c", "scratch")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")
	gitCommand(t, dir, "config", "--local", "kit.git.base", "integration")
	gitCommand(t, dir, "config", "--local", "kit.git.source", "scratch")

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"git", "compare", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	var got compareResult
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Base != "integration" || got.Source != "scratch" || got.Pending != 1 {
		t.Fatalf("repository config was not applied: %#v", got)
	}
}

func TestGitSyncConflictPromptAbortKeepsWork(t *testing.T) {
	dir := appRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "push", "-u", "origin", "develop")
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "work conflict")
	originalWork := gitCommandOutput(t, dir, "rev-parse", "work")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "team conflict")
	gitCommand(t, dir, "push", "origin", "develop")
	gitCommand(t, dir, "switch", "work")

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader("a\n"), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	err := a.Run(context.Background(), []string{"git", "sync", "--cwd", dir, "--yes"})
	if clierror.Code(err) != clierror.Conflict || !strings.Contains(err.Error(), "was aborted") {
		t.Fatalf("expected explicit sync abort, got %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "[c]ontinue, [s]kip, [a]bort") {
		t.Fatalf("sync conflict prompt was not shown:\n%s", output.String())
	}
	if got := gitCommandOutput(t, dir, "rev-parse", "work"); got != originalWork {
		t.Fatalf("work changed after sync abort: got %s want %s", got, originalWork)
	}
}

func TestSyncDryRunReportsExcludedMergesInJSONAndHumanOutput(t *testing.T) {
	dir := appRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "push", "-u", "origin", "develop")
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "work.txt")
	gitCommand(t, dir, "commit", "-m", "work")
	gitCommand(t, dir, "switch", "-c", "side")
	if err := os.WriteFile(filepath.Join(dir, "side.txt"), []byte("side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "side.txt")
	gitCommand(t, dir, "commit", "-m", "side")
	gitCommand(t, dir, "switch", "work")
	gitCommand(t, dir, "merge", "--no-ff", "side", "-m", "merge side")

	var human bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &human, ErrOut: &human}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"sync", "--dry-run", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human.String(), "Excluded") || !strings.Contains(human.String(), "자동 backup") {
		t.Fatalf("dry-run did not warn about excluded merge topology:\n%s", human.String())
	}

	var jsonOutput bytes.Buffer
	a = &Application{IO: IO{In: strings.NewReader(""), Out: &jsonOutput, ErrOut: &jsonOutput}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"sync", "--dry-run", "--json", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	var result struct {
		ExcludedMerges int `json:"excluded_merge_commits"`
	}
	if err := json.Unmarshal(jsonOutput.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExcludedMerges != 1 {
		t.Fatalf("excluded merge count = %d, want 1", result.ExcludedMerges)
	}

	var actualJSON, warnings bytes.Buffer
	a = &Application{IO: IO{In: strings.NewReader(""), Out: &actualJSON, ErrOut: &warnings}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"sync", "--yes", "--json", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warnings.String(), "side-parent") || !strings.Contains(warnings.String(), "자동 backup") {
		t.Fatalf("--yes did not emit exclusion warning to stderr:\n%s", warnings.String())
	}
	if err := json.Unmarshal(actualJSON.Bytes(), &result); err != nil {
		t.Fatalf("--yes JSON stdout is not a single result: %v\n%s", err, actualJSON.String())
	}
	if result.ExcludedMerges != 1 {
		t.Fatalf("actual excluded merge count = %d, want 1", result.ExcludedMerges)
	}
}

func TestSyncCleansOnlyMarkedMergedCurrentReviewBranch(t *testing.T) {
	dir := appRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "push", "-u", "origin", "develop")
	gitCommand(t, dir, "switch", "-c", "work")
	gitCommand(t, dir, "switch", "develop")
	gitCommand(t, dir, "switch", "-c", "review/merged")
	if err := os.WriteFile(filepath.Join(dir, "review.txt"), []byte("review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "review.txt")
	gitCommand(t, dir, "commit", "-m", "review")
	gitCommand(t, dir, "push", "-u", "origin", "review/merged")
	gitCommand(t, dir, "switch", "develop")
	gitCommand(t, dir, "merge", "--ff-only", "review/merged")
	gitCommand(t, dir, "push", "origin", "develop")
	gitCommand(t, dir, "branch", "-f", "work", "develop")
	gitCommand(t, dir, "config", "--local", "branch.review/merged.kitCreated", "true")
	gitCommand(t, dir, "switch", "review/merged")

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"sync", "--yes", "--json", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Cleaned []string `json:"cleaned_branches"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Cleaned) != 1 || result.Cleaned[0] != "review/merged" {
		t.Fatalf("unexpected cleaned branches: %#v", result.Cleaned)
	}
	assertBranchMissing(t, dir, "review/merged")
	if got := gitCommandOutput(t, dir, "branch", "--show-current"); got != "work" {
		t.Fatalf("cleanup did not switch from current review branch to work: %s", got)
	}
	if !commandSucceeds(dir, "show-ref", "--verify", "--quiet", "refs/remotes/origin/review/merged") {
		t.Fatal("cleanup removed the remote tracking ref")
	}
	if commandSucceeds(dir, "config", "--local", "--get", "branch.review/merged.kitCreated") {
		t.Fatal("marker remained after branch cleanup")
	}
}

func TestSyncPreservesUnmarkedProtectedAndNonAncestorBranches(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	gitCommand(t, dir, "switch", "develop")
	gitCommand(t, dir, "branch", "review/unmarked")
	gitCommand(t, dir, "switch", "-c", "review/unmerged")
	if err := os.WriteFile(filepath.Join(dir, "unmerged.txt"), []byte("unmerged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "unmerged.txt")
	gitCommand(t, dir, "commit", "-m", "unmerged")
	gitCommand(t, dir, "config", "--local", "branch.review/unmerged.kitCreated", "true")
	gitCommand(t, dir, "config", "--local", "branch.work.kitCreated", "true")

	cleaned, warnings := cleanupSyncedKitBranches(context.Background(), gitservice.Service{Dir: dir}, gitservice.DefaultWorkflowConfig())
	if len(cleaned) != 0 || len(warnings) != 0 {
		t.Fatalf("unexpected cleanup for preserved branches: cleaned=%#v warnings=%#v", cleaned, warnings)
	}
	for _, branch := range []string{"review/unmarked", "review/unmerged", "work"} {
		assertBranchExists(t, dir, branch)
	}
}

func TestSyncDoesNotCleanMarkedBranchDuringDryRunBaseOnlyOrFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		dirty bool
	}{
		{name: "dry-run", args: []string{"sync", "--dry-run", "--json"}},
		{name: "base-only", args: []string{"sync", "--base-only", "--yes", "--json"}},
		{name: "workflow failure", args: []string{"sync", "--yes", "--json"}, dirty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := syncCleanupRepository(t)
			gitCommand(t, dir, "branch", "review/marked")
			gitCommand(t, dir, "config", "--local", "branch.review/marked.kitCreated", "true")
			if tc.dirty {
				if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
			args := append(append([]string(nil), tc.args...), "--cwd", dir)
			err := a.Run(context.Background(), args)
			if tc.dirty && err == nil {
				t.Fatal("sync succeeded despite workflow planning failure")
			}
			if !tc.dirty && err != nil {
				t.Fatal(err)
			}
			assertBranchExists(t, dir, "review/marked")
			if marked, markerErr := (gitservice.Service{Dir: dir}).IsKitCreatedBranch(context.Background(), "review/marked"); markerErr != nil || !marked {
				t.Fatalf("marker changed without successful cleanup: marked=%v err=%v", marked, markerErr)
			}
		})
	}
}

func TestSyncPreservesCurrentMergedBranchWithUpstreamMismatch(t *testing.T) {
	dir := syncCleanupRepository(t)
	gitCommand(t, dir, "switch", "-c", "review/mismatch")
	gitCommand(t, dir, "config", "--local", "branch.review/mismatch.kitCreated", "true")
	gitCommand(t, dir, "config", "--local", "branch.review/mismatch.remote", "other")
	gitCommand(t, dir, "config", "--local", "branch.review/mismatch.merge", "refs/heads/review/mismatch")

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"sync", "--yes", "--json", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Cleaned  []string `json:"cleaned_branches"`
		Warnings []string `json:"cleanup_warnings"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Cleaned) != 0 || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "upstream") {
		t.Fatalf("unexpected mismatch cleanup result: %#v", result)
	}
	assertBranchExists(t, dir, "review/mismatch")
	if got := gitCommandOutput(t, dir, "branch", "--show-current"); got != "review/mismatch" {
		t.Fatalf("cleanup switched an upstream-mismatched branch: %s", got)
	}
}

func TestSyncPreservesMarkedSquashEquivalentBranch(t *testing.T) {
	dir := syncCleanupRepository(t)
	gitCommand(t, dir, "switch", "-c", "review/squash")
	if err := os.WriteFile(filepath.Join(dir, "squash.txt"), []byte("same patch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "squash.txt")
	gitCommand(t, dir, "commit", "-m", "review change")
	gitCommand(t, dir, "config", "--local", "branch.review/squash.kitCreated", "true")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "squash.txt"), []byte("same patch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "squash.txt")
	gitCommand(t, dir, "commit", "-m", "squash equivalent")
	gitCommand(t, dir, "push", "origin", "develop")

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"sync", "--yes", "--json", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Cleaned []string `json:"cleaned_branches"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Cleaned) != 0 {
		t.Fatalf("squash-equivalent branch was cleaned without ancestry: %#v", result.Cleaned)
	}
	assertBranchExists(t, dir, "review/squash")
}

func TestSyncSafeDeleteFailureIsWarningAndDoesNotFailSync(t *testing.T) {
	dir := syncCleanupRepository(t)
	gitCommand(t, dir, "branch", "review/delete-failure")
	gitCommand(t, dir, "config", "--local", "branch.review/delete-failure.kitCreated", "true")
	var output bytes.Buffer
	runner := &syncCleanupRunner{Runner: gitservice.ExecRunner{}, failDelete: "review/delete-failure"}
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Git:   func(dir string) gitservice.Service { return gitservice.Service{Dir: dir, Runner: runner} },
		Build: buildinfo.Current(),
	}
	if err := a.Run(context.Background(), []string{"sync", "--yes", "--json", "--cwd", dir}); err != nil {
		t.Fatalf("safe deletion failure changed sync outcome: %v", err)
	}
	var result struct {
		Warnings []string `json:"cleanup_warnings"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "safe local branch deletion failed") {
		t.Fatalf("safe delete warning missing: %#v", result.Warnings)
	}
	assertBranchExists(t, dir, "review/delete-failure")
}

func TestCleanupPreservesSameNameBranchRecreatedDuringInspection(t *testing.T) {
	dir := syncCleanupRepository(t)
	gitCommand(t, dir, "branch", "review/recreated")
	gitCommand(t, dir, "config", "--local", "branch.review/recreated.kitCreated", "true")
	gitCommand(t, dir, "switch", "-c", "replacement")
	gitCommand(t, dir, "commit", "--allow-empty", "-m", "replacement")
	replacementTip := gitCommandOutput(t, dir, "rev-parse", "HEAD")
	gitCommand(t, dir, "switch", "develop")
	runner := &syncCleanupRunner{Runner: gitservice.ExecRunner{}, replaceBranch: "review/recreated", replaceHash: replacementTip}
	cleaned, warnings := cleanupSyncedKitBranches(context.Background(), gitservice.Service{Dir: dir, Runner: runner}, gitservice.DefaultWorkflowConfig())
	if len(cleaned) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "tip changed") {
		t.Fatalf("recreated branch was not preserved: cleaned=%#v warnings=%#v", cleaned, warnings)
	}
	assertBranchExists(t, dir, "review/recreated")
}

type syncCleanupRunner struct {
	gitservice.Runner
	failDelete    string
	replaceBranch string
	replaceHash   string
	revisionReads int
}

func (r *syncCleanupRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if len(args) == 3 && args[0] == "branch" && args[1] == "-d" && args[2] == r.failDelete {
		return nil, errors.New("injected safe delete failure")
	}
	if len(args) == 3 && args[0] == "rev-parse" && args[1] == "--verify" && args[2] == r.replaceBranch+"^{commit}" {
		r.revisionReads++
		if r.revisionReads == 2 {
			if _, err := r.Runner.Run(ctx, dir, "update-ref", "refs/heads/"+r.replaceBranch, r.replaceHash); err != nil {
				return nil, err
			}
		}
	}
	return r.Runner.Run(ctx, dir, args...)
}

func (r *syncCleanupRunner) RunInput(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error) {
	return r.Runner.RunInput(ctx, dir, input, args...)
}

func syncCleanupRepository(t *testing.T) string {
	t.Helper()
	dir := appRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "push", "-u", "origin", "develop")
	gitCommand(t, dir, "switch", "-c", "work")
	gitCommand(t, dir, "switch", "develop")
	return dir
}

func TestCompareAndPickWarnWhenSourceMergesAreExcluded(t *testing.T) {
	var compareOutput bytes.Buffer
	a := &Application{IO: IO{Out: &compareOutput}}
	a.printCompare(compareResult{Source: "work", Base: "develop", ExcludedMerges: 1}, false)
	if !strings.Contains(compareOutput.String(), "first-parent 후보") {
		t.Fatalf("compare did not warn about excluded merges:\n%s", compareOutput.String())
	}

	var pickOutput bytes.Buffer
	a = &Application{IO: IO{Out: &pickOutput}}
	a.printPickSummary(pickOptions{target: "feat/test", source: "work", base: "develop", excludedMerges: 1}, nil)
	if !strings.Contains(pickOutput.String(), "선택 후보에 포함되지 않습니다") {
		t.Fatalf("pick did not warn about excluded merges:\n%s", pickOutput.String())
	}
}

func TestReadStringContextCancelsBlockedConflictResolverRead(t *testing.T) {
	reader := &contextBlockingReader{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := readStringContext(ctx, bufio.NewReader(reader))
		result <- err
	}()
	<-reader.started
	cancel()
	err := <-result
	close(reader.release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled conflict read, got %v", err)
	}
}

type contextBlockingReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *contextBlockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, io.EOF
}

func TestSyncCommandErrorMapsCanceledMutationToInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, operationErr := range []error{nil, errors.New("conflict cleanup completed")} {
		err := syncCommandError(ctx, operationErr)
		if clierror.Code(err) != clierror.Interrupt {
			t.Fatalf("expected interrupt code for %v, got %v", operationErr, err)
		}
	}
}

func TestConfirmReaderContextCancelsBlockedRead(t *testing.T) {
	reader := &contextBlockingReader{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := confirmReaderContext(ctx, bufio.NewReader(reader), io.Discard, "confirm: ")
		result <- err
	}()
	<-reader.started
	cancel()
	err := <-result
	close(reader.release)
	if clierror.Code(err) != clierror.Interrupt {
		t.Fatalf("expected interrupt while confirmation read was blocked, got %v", err)
	}
}

func TestPickRejectsStaleWorkBeforeSelection(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "team.txt"), []byte("team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "team.txt")
	gitCommand(t, dir, "commit", "-m", "team change")

	selected := false
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard},
		Build: buildinfo.Current(),
		Select: func([]selector.Item, string) ([]selector.Item, error) {
			selected = true
			return nil, nil
		},
	}
	err := a.Run(context.Background(), []string{"pick", "feat/stale", "--local", "--from", "work", "--cwd", dir})
	if clierror.Code(err) != clierror.Conflict || !strings.Contains(err.Error(), "kit sync") {
		t.Fatalf("expected stale work conflict, got %v", err)
	}
	if selected {
		t.Fatal("selector opened for stale work")
	}
}

func TestConfigInitWritesRepositoryDefaults(t *testing.T) {
	dir := appRepository(t)
	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"config", "init", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	if got := gitCommandOutput(t, dir, "config", "--local", "--get", "kit.git.base"); got != "develop" {
		t.Fatalf("unexpected configured base: %s", got)
	}
	if got := gitCommandOutput(t, dir, "config", "--local", "--get", "kit.git.provider"); got != "gitea" {
		t.Fatalf("unexpected configured provider: %s", got)
	}
}

func TestGitPublishPushesOnlyReviewBranch(t *testing.T) {
	dir := appRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "switch", "-c", "feat/publish")
	if err := os.WriteFile(filepath.Join(dir, "publish.txt"), []byte("publish\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "publish.txt")
	gitCommand(t, dir, "commit", "-m", "publish feature")

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"git", "publish", "--cwd", dir, "--yes", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !commandSucceeds(remote, "show-ref", "--verify", "--quiet", "refs/heads/feat/publish") {
		t.Fatal("review branch was not pushed")
	}
	output.Reset()
	gitCommand(t, dir, "switch", "develop")
	err := a.Run(context.Background(), []string{"git", "publish", "--cwd", dir, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "protected workflow branch") {
		t.Fatalf("expected protected branch rejection, got %v", err)
	}
}

func appRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCommand(t, dir, "init", "-b", "develop")
	gitCommand(t, dir, "config", "user.name", "Kit Test")
	gitCommand(t, dir, "config", "user.email", "kit@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "root")
	return dir
}

func gitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitCommandOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func assertBranchMissing(t *testing.T, dir, branch string) {
	t.Helper()
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = dir
	if err := cmd.Run(); err == nil {
		t.Fatalf("branch %s unexpectedly exists", branch)
	}
}

func commandSucceeds(dir string, args ...string) bool {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run() == nil
}

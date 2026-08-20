package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kit/internal/buildinfo"
	"kit/internal/clierror"
	gitservice "kit/internal/git"
)

func TestWorkCleanupDryRunDefaultsToAutomaticBackups(t *testing.T) {
	dir := workBackupRepository(t)
	automatic := "kit/backup/work-20260820-120000-abcdef12"
	manual := "kit/backup/work-manual-abcdef12"
	beforeRestore := "kit/backup/work-before-restore-20260820-120000-abcdef12"
	for _, branch := range []string{automatic, manual, beforeRestore, "kit/recovery/work-kept", "kit/tmp/work-kept"} {
		gitCommand(t, dir, "branch", branch)
	}

	var output bytes.Buffer
	application := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := application.Run(context.Background(), []string{"git", "work", "cleanup", "--dry-run", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, automatic) || strings.Contains(got, manual) || strings.Contains(got, beforeRestore) {
		t.Fatalf("default cleanup selected unexpected branches: %q", got)
	}
	for _, branch := range []string{automatic, manual, beforeRestore, "kit/recovery/work-kept", "kit/tmp/work-kept"} {
		assertBranchExists(t, dir, branch)
	}
}

func TestWorkCleanupAllDeletesOnlyBackupNamespace(t *testing.T) {
	dir := workBackupRepository(t)
	branches := []string{
		"kit/backup/work-20260820-120000-abcdef12",
		"kit/backup/work-manual-abcdef12",
		"kit/backup/work-before-restore-20260820-120000-abcdef12",
	}
	for _, branch := range append(append([]string{}, branches...), "kit/recovery/work-kept", "kit/tmp/work-kept") {
		gitCommand(t, dir, "branch", branch)
	}

	var output bytes.Buffer
	application := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := application.Run(context.Background(), []string{"git", "work", "cleanup", "--all", "--yes", "--json", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	var result workCleanupResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != len(branches) || !result.All || result.DryRun {
		t.Fatalf("unexpected cleanup result: %#v", result)
	}
	for _, branch := range branches {
		assertBranchMissing(t, dir, branch)
	}
	assertBranchExists(t, dir, "kit/recovery/work-kept")
	assertBranchExists(t, dir, "kit/tmp/work-kept")
}

func TestWorkCleanupRequiresConfirmation(t *testing.T) {
	dir := workBackupRepository(t)
	automatic := "kit/backup/work-20260820-120000-abcdef12"
	gitCommand(t, dir, "branch", automatic)

	var output bytes.Buffer
	application := &Application{IO: IO{In: strings.NewReader("n\n"), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := application.Run(context.Background(), []string{"git", "work", "cleanup", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	assertBranchExists(t, dir, automatic)
	if !strings.Contains(output.String(), "취소되었습니다.") {
		t.Fatalf("missing cancellation message: %q", output.String())
	}
}

func TestWorkCleanupReportsPartialFailureAndRemainingBranch(t *testing.T) {
	dir := workBackupRepository(t)
	first := "kit/backup/work-20260820-120000-abcdef12"
	failed := "kit/backup/work-20260820-120001-fedcba98"
	gitCommand(t, dir, "branch", first)
	gitCommand(t, dir, "branch", failed)
	runner := &appFailRunner{Runner: gitservice.ExecRunner{}, failDelete: failed}

	application := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}},
		Git:   func(dir string) gitservice.Service { return gitservice.Service{Dir: dir, Runner: runner} },
		Build: buildinfo.Current(),
	}
	err := application.Run(context.Background(), []string{"git", "work", "cleanup", "--yes", "--cwd", dir})
	if clierror.Code(err) != clierror.Failure || !strings.Contains(err.Error(), failed) {
		t.Fatalf("expected named partial cleanup failure, got %v", err)
	}
	assertBranchMissing(t, dir, first)
	assertBranchExists(t, dir, failed)
}

func TestWorkCleanupJSONReportsDeletedAndFailedBranchesOnPartialFailure(t *testing.T) {
	dir := workBackupRepository(t)
	first, err := gitservice.FormatWorkBackupRef("work", gitservice.WorkBackupAuto, "20260820-120000-abcdef12")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := gitservice.FormatWorkBackupRef("work", gitservice.WorkBackupAuto, "20260820-120001-fedcba98")
	if err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "branch", first)
	gitCommand(t, dir, "branch", failed)
	runner := &appFailRunner{Runner: gitservice.ExecRunner{}, failDelete: failed}

	var output bytes.Buffer
	application := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &bytes.Buffer{}},
		Git:   func(dir string) gitservice.Service { return gitservice.Service{Dir: dir, Runner: runner} },
		Build: buildinfo.Current(),
	}
	runErr := application.Run(context.Background(), []string{"git", "work", "cleanup", "--yes", "--json", "--cwd", dir})
	if clierror.Code(runErr) != clierror.Failure {
		t.Fatalf("expected partial cleanup failure, got %v", runErr)
	}
	var result workCleanupResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("partial failure did not return JSON: %v; output=%q", err, output.String())
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != first || result.Failed[failed] == "" {
		t.Fatalf("unexpected partial cleanup result: %#v", result)
	}
}

func TestWorkCleanupReportsAndPreservesCheckedOutBackup(t *testing.T) {
	dir := workBackupRepository(t)
	deleted := "kit/backup/work-20260820-120000-abcdef12"
	checkedOut := "kit/backup/work-20260820-120001-fedcba98"
	gitCommand(t, dir, "branch", deleted)
	gitCommand(t, dir, "switch", "-c", checkedOut)

	application := &Application{IO: IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}, Build: buildinfo.Current()}
	err := application.Run(context.Background(), []string{"git", "work", "cleanup", "--yes", "--cwd", dir})
	if clierror.Code(err) != clierror.Failure || !strings.Contains(err.Error(), checkedOut) {
		t.Fatalf("expected checked-out backup partial failure, got %v", err)
	}
	assertBranchMissing(t, dir, deleted)
	assertBranchExists(t, dir, checkedOut)
	if got := gitCommandOutput(t, dir, "branch", "--show-current"); got != checkedOut {
		t.Fatalf("cleanup moved the current checkout: %s", got)
	}
}

func TestWorkCleanupDefaultSelectsV2AutoOnly(t *testing.T) {
	dir := workBackupRepository(t)
	automatic, err := gitservice.FormatWorkBackupRef("work", gitservice.WorkBackupAuto, "20260820-120000-abcdef12")
	if err != nil {
		t.Fatal(err)
	}
	manual, err := gitservice.FormatWorkBackupRef("work", gitservice.WorkBackupManual, "abcdef12")
	if err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "branch", automatic)
	gitCommand(t, dir, "branch", manual)

	var output bytes.Buffer
	application := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := application.Run(context.Background(), []string{"git", "work", "cleanup", "--dry-run", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, automatic) || strings.Contains(got, manual) {
		t.Fatalf("default v2 cleanup selected unexpected refs: %q", got)
	}
}

func TestWorkCleanupV2DoesNotCrossDeleteCollidingLegacySourceNames(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "config", "--local", "kit.git.source", "a/b")
	owned, err := gitservice.FormatWorkBackupRef("a/b", gitservice.WorkBackupAuto, "20260820-120000-abcdef12")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := gitservice.FormatWorkBackupRef("a-b", gitservice.WorkBackupAuto, "20260820-120000-abcdef12")
	if err != nil {
		t.Fatal(err)
	}
	spoofed := owned + "-suffix"
	legacyCollision := "kit/backup/a-b-20260820-120000-abcdef12"
	for _, branch := range []string{owned, foreign, spoofed, legacyCollision} {
		gitCommand(t, dir, "branch", branch)
	}

	application := &Application{IO: IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}, Build: buildinfo.Current()}
	if err := application.Run(context.Background(), []string{"git", "work", "cleanup", "--all", "--yes", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	assertBranchMissing(t, dir, owned)
	for _, branch := range []string{foreign, spoofed, legacyCollision} {
		assertBranchExists(t, dir, branch)
	}
}

func TestWorkRestoreRejectsForeignAndMalformedV2BackupRefs(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "branch", "work")
	foreign, err := gitservice.FormatWorkBackupRef("a-b", gitservice.WorkBackupManual, "abcdef12")
	if err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "branch", foreign)
	application := &Application{IO: IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}}, Build: buildinfo.Current()}
	for _, candidate := range []string{foreign, foreign + "-suffix"} {
		err := application.Run(context.Background(), []string{"git", "work", "restore", candidate, "--yes", "--cwd", dir})
		if clierror.Code(err) != clierror.Usage {
			t.Fatalf("expected ownership validation for %q, got %v", candidate, err)
		}
	}
}

func TestWorkRestoreFailureRollsBackAndRemovesTemporarySafetyBackup(t *testing.T) {
	dir, restoreBackup, originalWork, targetHash := restoreBackupRepository(t)
	runner := &appFailRunner{Runner: gitservice.ExecRunner{}, failForceBranch: "work", failForceHash: targetHash}
	var output bytes.Buffer
	application := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Git:   func(dir string) gitservice.Service { return gitservice.Service{Dir: dir, Runner: runner} },
		Build: buildinfo.Current(),
	}
	err := application.Run(context.Background(), []string{"git", "work", "restore", restoreBackup, "--yes", "--cwd", dir})
	if clierror.Code(err) != clierror.Failure || !strings.Contains(err.Error(), "temporary safety backup") || !strings.Contains(err.Error(), "was removed") {
		t.Fatalf("expected verified restore rollback, got %v", err)
	}
	if got := gitCommandOutput(t, dir, "rev-parse", "work"); got != originalWork {
		t.Fatalf("work was not rolled back: got %s want %s", got, originalWork)
	}
	if got := gitCommandOutput(t, dir, "branch", "--show-current"); got != "work" {
		t.Fatalf("checkout was not restored: %s", got)
	}
	if refs := beforeRestoreRefs(t, dir); len(refs) != 0 {
		t.Fatalf("failed restore left temporary safety backup: %s", refs)
	}
}

func TestWorkRestoreSuccessRetainsSafetyBackup(t *testing.T) {
	dir, restoreBackup, originalWork, targetHash := restoreBackupRepository(t)
	var output bytes.Buffer
	application := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := application.Run(context.Background(), []string{"git", "work", "restore", restoreBackup, "--yes", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if got := gitCommandOutput(t, dir, "rev-parse", "work"); got != targetHash {
		t.Fatalf("work was not restored: got %s want %s", got, targetHash)
	}
	refs := beforeRestoreRefs(t, dir)
	if len(refs) == 0 {
		t.Fatal("successful restore did not retain its safety backup")
	}
	if got := gitCommandOutput(t, dir, "rev-parse", refs[0]); got != originalWork {
		t.Fatalf("safety backup does not point to original work: got %s want %s", got, originalWork)
	}
}

type appFailRunner struct {
	gitservice.Runner
	failDelete      string
	failForceBranch string
	failForceHash   string
	forceFailed     bool
}

func (r *appFailRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if len(args) == 3 && args[0] == "branch" && args[1] == "-D" && args[2] == r.failDelete {
		return nil, errors.New("injected delete failure")
	}
	if !r.forceFailed && len(args) == 4 && args[0] == "branch" && args[1] == "-f" && args[2] == r.failForceBranch && args[3] == r.failForceHash {
		r.forceFailed = true
		return nil, errors.New("injected force-branch failure")
	}
	return r.Runner.Run(ctx, dir, args...)
}

func (r *appFailRunner) RunInput(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error) {
	return r.Runner.RunInput(ctx, dir, input, args...)
}

func workBackupRepository(t *testing.T) string {
	t.Helper()
	dir := appRepository(t)
	gitCommand(t, dir, "branch", "work")
	return dir
}

func restoreBackupRepository(t *testing.T) (string, string, string, string) {
	t.Helper()
	dir := appRepository(t)
	targetHash := gitCommandOutput(t, dir, "rev-parse", "develop")
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "work.txt")
	gitCommand(t, dir, "commit", "-m", "work")
	originalWork := gitCommandOutput(t, dir, "rev-parse", "work")
	restoreBackup := "kit/backup/work-manual-" + targetHash[:8]
	gitCommand(t, dir, "branch", restoreBackup, targetHash)
	return dir, restoreBackup, originalWork, targetHash
}

func assertBranchExists(t *testing.T, dir, branch string) {
	t.Helper()
	if !commandSucceeds(dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch) {
		t.Fatalf("branch %s does not exist", branch)
	}
}

func beforeRestoreRefs(t *testing.T, dir string) []string {
	t.Helper()
	all := gitCommandOutput(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/kit/backup")
	var refs []string
	for _, ref := range strings.Fields(all) {
		parsed, ok := gitservice.ParseWorkBackupRef(ref, "work")
		if ok && parsed.Kind == gitservice.WorkBackupBeforeRestore {
			refs = append(refs, ref)
		}
	}
	return refs
}

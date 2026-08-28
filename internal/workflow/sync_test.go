package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	gitservice "kit/internal/git"
)

func TestSyncFastForwardsBaseAndRebuildsWorkWithPendingCommits(t *testing.T) {
	dir, remote := syncRepository(t)
	root := output(t, dir, "rev-parse", "develop")

	run(t, dir, "switch", "-c", "work")
	writeCommit(t, dir, "applied.txt", "applied\n", "applied work")
	applied := output(t, dir, "rev-parse", "HEAD")
	writeCommit(t, dir, "pending.txt", "pending\n", "pending work")

	run(t, dir, "switch", "develop")
	run(t, dir, "cherry-pick", "-x", applied)
	writeCommit(t, dir, "team.txt", "team\n", "team merge")
	run(t, dir, "push", "origin", "develop")
	run(t, dir, "switch", "work")
	run(t, dir, "branch", "-f", "develop", root)

	service := gitservice.Service{Dir: dir}
	result, err := Sync(context.Background(), service, SyncOptions{Config: gitservice.DefaultWorkflowConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if !result.BaseUpdated || !result.SourceRebuilt || result.AppliedDropped != 1 || result.PendingKept != 1 || result.BackupBranch == "" {
		t.Fatalf("unexpected sync result: %#v", result)
	}
	if got := output(t, dir, "branch", "--show-current"); got != "work" {
		t.Fatalf("checkout not restored: %s", got)
	}
	if ok := commandOK(dir, "merge-base", "--is-ancestor", "develop", "work"); !ok {
		t.Fatal("rebuilt work does not contain develop")
	}
	if got := output(t, dir, "log", "--format=%s", "develop..work"); got != "pending work" {
		t.Fatalf("unexpected rebuilt work commits: %s", got)
	}
	if !commandOK(dir, "show-ref", "--verify", "--quiet", "refs/heads/"+result.BackupBranch) {
		t.Fatalf("backup branch missing: %s", result.BackupBranch)
	}
	remoteHead := output(t, remote, "rev-parse", "refs/heads/develop")
	if got := output(t, dir, "rev-parse", "develop"); got != remoteHead {
		t.Fatalf("develop not fast-forwarded: got %s want %s", got, remoteHead)
	}
}

func TestSyncAllowsCleanDevelopIntegrationAgainstFetchedBase(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		t.Run("dry-run="+strconv.FormatBool(dryRun), func(t *testing.T) {
			dir, _ := syncRepository(t)
			baseBefore := output(t, dir, "rev-parse", "develop")
			run(t, dir, "switch", "-c", "work")
			writeCommit(t, dir, "work.txt", "work\n", "work change")
			run(t, dir, "switch", "develop")
			writeCommit(t, dir, "team.txt", "team\n", "team change")
			run(t, dir, "push", "origin", "develop")
			run(t, dir, "switch", "work")
			run(t, dir, "merge", "--no-ff", "develop", "-m", "integrate develop")
			run(t, dir, "branch", "-f", "develop", baseBefore)
			workBefore := output(t, dir, "rev-parse", "work")

			result, err := Sync(context.Background(), gitservice.Service{Dir: dir}, SyncOptions{
				Config: gitservice.DefaultWorkflowConfig(),
				DryRun: dryRun,
			})
			if err != nil {
				t.Fatalf("clean integration was rejected: %v", err)
			}
			if result.PendingKept != 1 || result.AppliedDropped != 0 {
				t.Fatalf("pending work commit was not preserved: %#v", result)
			}
			if !dryRun && !commandOK(dir, "merge-base", "--is-ancestor", "develop", "work") {
				t.Fatalf("actual sync did not preserve integrated base in work: %#v", result)
			}
			if !dryRun {
				if output(t, dir, "rev-parse", "work") == workBefore {
					t.Fatalf("sync did not replace the integration merge")
				}
				if got := output(t, dir, "log", "--format=%s", "develop..work"); got != "work change" {
					t.Fatalf("sync did not preserve only the first-parent pending commit: %q", got)
				}
			}
		})
	}
}

func TestRefreshAllowsCleanDevelopIntegration(t *testing.T) {
	dir, _ := syncRepository(t)
	run(t, dir, "switch", "-c", "work")
	writeCommit(t, dir, "work.txt", "work\n", "work change")
	run(t, dir, "switch", "develop")
	writeCommit(t, dir, "team.txt", "team\n", "team change")
	run(t, dir, "switch", "work")
	run(t, dir, "merge", "--no-ff", "develop", "-m", "integrate develop")

	result, err := Refresh(context.Background(), gitservice.Service{Dir: dir}, gitservice.DefaultWorkflowConfig(), false)
	if err != nil {
		t.Fatalf("clean integration was rejected: %v", err)
	}
	if !result.SourceRebuilt || result.PendingKept != 1 || result.AppliedDropped != 0 {
		t.Fatalf("pending work commit was not preserved: %#v", result)
	}
	if got := output(t, dir, "log", "--format=%s", "develop..work"); got != "work change" {
		t.Fatalf("refresh did not preserve only the first-parent pending commit: %q", got)
	}
}

func TestSyncAllowsFeatureMergeWhenBaseIsAlreadyInSource(t *testing.T) {
	dir, _ := syncRepository(t)
	run(t, dir, "switch", "-c", "work")
	writeCommit(t, dir, "work.txt", "work\n", "work change")
	run(t, dir, "switch", "-c", "feature")
	writeCommit(t, dir, "feature.txt", "feature\n", "feature change")
	run(t, dir, "switch", "work")
	run(t, dir, "merge", "--no-ff", "feature", "-m", "merge feature")
	result, err := Sync(context.Background(), gitservice.Service{Dir: dir}, SyncOptions{Config: gitservice.DefaultWorkflowConfig()})
	if err != nil {
		t.Fatalf("feature merge was rejected although base is included: %v", err)
	}
	if !result.SourceRebuilt || result.PendingKept != 1 {
		t.Fatalf("merged feature source was not rebuilt: %#v", result)
	}
	if got := output(t, dir, "log", "--format=%s", "develop..work"); got != "work change" {
		t.Fatalf("sync reapplied side-parent or merge commit: %q", got)
	}
}

func TestSyncDropsProviderConfirmedCommitsAfterSquashMerge(t *testing.T) {
	t.Skip("sync now relies only on Git patch equivalence and preserves ambiguous squash commits")
	dir, _ := syncRepository(t)
	run(t, dir, "switch", "-c", "work")
	writeCommit(t, dir, "first.txt", "first\n", "first work")
	first := output(t, dir, "rev-parse", "HEAD")
	writeCommit(t, dir, "second.txt", "second\n", "second work")
	second := output(t, dir, "rev-parse", "HEAD")
	_ = first
	_ = second

	run(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "first.txt", "second.txt")
	run(t, dir, "commit", "-m", "squash work")
	run(t, dir, "push", "origin", "develop")
	run(t, dir, "switch", "work")

	service := gitservice.Service{Dir: dir}
	result, err := Sync(context.Background(), service, SyncOptions{Config: gitservice.DefaultWorkflowConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if result.AppliedDropped != 2 || result.PendingKept != 0 || !result.SourceRebuilt {
		t.Fatalf("unexpected sync result: %#v", result)
	}
	if got, want := output(t, dir, "rev-parse", "work"), output(t, dir, "rev-parse", "develop"); got != want {
		t.Fatalf("work was not rebuilt to the squashed base: got %s want %s", got, want)
	}
}

func TestSyncConflictWithoutResolverKeepsOriginalWork(t *testing.T) {
	dir, _ := syncRepository(t)
	run(t, dir, "switch", "-c", "work")
	writeCommit(t, dir, "root.txt", "work\n", "work conflict")
	originalWork := output(t, dir, "rev-parse", "work")

	run(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "root.txt")
	run(t, dir, "commit", "-m", "team conflict")
	run(t, dir, "push", "origin", "develop")
	run(t, dir, "switch", "work")

	service := gitservice.Service{Dir: dir}
	result, err := Sync(context.Background(), service, SyncOptions{Config: gitservice.DefaultWorkflowConfig()})
	if err == nil || !strings.Contains(err.Error(), "original work was kept") {
		t.Fatalf("expected safe conflict, got result=%#v err=%v", result, err)
	}
	if got := output(t, dir, "rev-parse", "work"); got != originalWork {
		t.Fatalf("work changed after failed rebuild: got %s want %s", got, originalWork)
	}
	if got := output(t, dir, "branch", "--show-current"); got != "work" {
		t.Fatalf("checkout not restored after conflict: %s", got)
	}
	if got := output(t, dir, "status", "--porcelain"); got != "" {
		t.Fatalf("working tree dirty after conflict: %s", got)
	}
	if refs := output(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/kit/backup"); refs != "" {
		t.Fatalf("verified rollback left its automatic backup behind: %s", refs)
	}
}

func TestSyncConflictRollsBackFastForwardedBaseAndRemovesBackup(t *testing.T) {
	dir, _ := syncRepository(t)
	baseBefore := output(t, dir, "rev-parse", "develop")
	run(t, dir, "switch", "-c", "work")
	writeCommit(t, dir, "root.txt", "work\n", "work conflict")
	originalWork := output(t, dir, "rev-parse", "work")

	run(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "root.txt")
	run(t, dir, "commit", "-m", "team conflict")
	run(t, dir, "push", "origin", "develop")
	run(t, dir, "switch", "work")
	run(t, dir, "branch", "-f", "develop", baseBefore)
	run(t, dir, "switch", "develop")

	service := gitservice.Service{Dir: dir}
	_, err := Sync(context.Background(), service, SyncOptions{Config: gitservice.DefaultWorkflowConfig()})
	if err == nil || !strings.Contains(err.Error(), "rollback backup") {
		t.Fatalf("expected verified rollback, got %v", err)
	}
	if got := output(t, dir, "rev-parse", "develop"); got != baseBefore {
		t.Fatalf("base was not rolled back: got %s want %s", got, baseBefore)
	}
	if got := output(t, dir, "rev-parse", "work"); got != originalWork {
		t.Fatalf("work was not rolled back: got %s want %s", got, originalWork)
	}
	if got := output(t, dir, "branch", "--show-current"); got != "develop" {
		t.Fatalf("checkout not restored: %s", got)
	}
	if refs := output(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/kit/backup"); refs != "" {
		t.Fatalf("automatic backup remained after verified rollback: %s", refs)
	}
}

func TestRefreshConflictRestoresWorkAndRemovesAutomaticBackup(t *testing.T) {
	dir, _ := syncConflictRepository(t)
	originalWork := output(t, dir, "rev-parse", "work")
	service := gitservice.Service{Dir: dir}
	_, err := Refresh(context.Background(), service, gitservice.DefaultWorkflowConfig(), false)
	if err == nil || !strings.Contains(err.Error(), "rollback backup") {
		t.Fatalf("expected refresh rollback, got %v", err)
	}
	if got := output(t, dir, "rev-parse", "work"); got != originalWork {
		t.Fatalf("refresh changed work after failure: got %s want %s", got, originalWork)
	}
	if got := output(t, dir, "branch", "--show-current"); got != "work" {
		t.Fatalf("refresh did not restore checkout: %s", got)
	}
	if refs := output(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/kit/backup"); refs != "" {
		t.Fatalf("refresh left its automatic rollback backup: %s", refs)
	}
}

func TestRefreshRebuildsMergedSourceFromFirstParentCommits(t *testing.T) {
	dir, _, workBefore := nonLinearSyncRepository(t)
	result, err := Refresh(context.Background(), gitservice.Service{Dir: dir}, gitservice.DefaultWorkflowConfig(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.SourceRebuilt || result.PendingKept != 1 || result.ExcludedMerges != 1 || result.BackupBranch == "" {
		t.Fatalf("unexpected refresh result: %#v", result)
	}
	if got := output(t, dir, "rev-parse", "work"); got == workBefore {
		t.Fatal("refresh did not rebuild merged work")
	}
	if got := output(t, dir, "log", "--format=%s", "develop..work"); got != "work change" {
		t.Fatalf("refresh reapplied side-parent or merge commit: %q", got)
	}
	workAfter := output(t, dir, "rev-parse", "work")
	backupBefore := output(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/kit/backup")
	again, err := Refresh(context.Background(), gitservice.Service{Dir: dir}, gitservice.DefaultWorkflowConfig(), false)
	if err != nil {
		t.Fatal(err)
	}
	if again.SourceRebuilt || again.BackupBranch != "" || output(t, dir, "rev-parse", "work") != workAfter || output(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/kit/backup") != backupBefore {
		t.Fatalf("repeat refresh was not a no-op: %#v", again)
	}
}

func TestSyncRebuildsMergedSourceFromFirstParentCommits(t *testing.T) {
	dir, _, workBefore := nonLinearSyncRepository(t)
	result, err := Sync(context.Background(), gitservice.Service{Dir: dir}, SyncOptions{Config: gitservice.DefaultWorkflowConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if !result.SourceRebuilt || result.PendingKept != 1 || result.ExcludedMerges != 1 || result.BackupBranch == "" {
		t.Fatalf("unexpected sync result: %#v", result)
	}
	if got := output(t, dir, "rev-parse", "work"); got == workBefore {
		t.Fatal("sync did not rebuild merged work")
	}
	if got := output(t, dir, "log", "--format=%s", "develop..work"); got != "work change" {
		t.Fatalf("sync reapplied side-parent or merge commit: %q", got)
	}
	workAfter := output(t, dir, "rev-parse", "work")
	backupBefore := output(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/kit/backup")
	again, err := Sync(context.Background(), gitservice.Service{Dir: dir}, SyncOptions{Config: gitservice.DefaultWorkflowConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if again.SourceRebuilt || again.BackupBranch != "" || output(t, dir, "rev-parse", "work") != workAfter || output(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/kit/backup") != backupBefore {
		t.Fatalf("repeat sync was not a no-op: %#v", again)
	}
}

func TestSyncCleanupFailureRetainsNamedBackup(t *testing.T) {
	dir, _ := syncConflictRepository(t)
	service := gitservice.Service{
		Dir: dir,
		Runner: failBranchDeleteRunner{
			Runner: gitservice.ExecRunner{},
			match:  "kit/backup/",
		},
	}
	_, err := Sync(context.Background(), service, SyncOptions{Config: gitservice.DefaultWorkflowConfig()})
	if err == nil || !strings.Contains(err.Error(), "rollback backup remains at kit/backup/v2/") {
		t.Fatalf("expected retained backup guidance, got %v", err)
	}
	refs := output(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/kit/backup")
	if !strings.HasPrefix(refs, "kit/backup/v2/") {
		t.Fatalf("failed cleanup did not retain backup: %q", refs)
	}
}

func TestSyncConflictContinueRebuildsWorkAfterResolution(t *testing.T) {
	dir, _ := syncConflictRepository(t)
	service := gitservice.Service{Dir: dir}
	resolved := false
	result, err := Sync(context.Background(), service, SyncOptions{
		Config: gitservice.DefaultWorkflowConfig(),
		Resolver: func(_ context.Context, conflict SyncConflict) (SyncConflictAction, error) {
			if resolved {
				t.Fatalf("continue failed after staging resolution: %v", conflict.ContinueErr)
			}
			if len(conflict.Unresolved) == 0 {
				t.Fatalf("unexpected resolver state: %#v", conflict)
			}
			if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("resolved\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, dir, "add", "root.txt")
			resolved = true
			return SyncConflictContinue, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resolved || !result.SourceRebuilt || result.PendingKept != 1 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %#v", result)
	}
	if got := output(t, dir, "show", "work:root.txt"); got != "resolved" {
		t.Fatalf("resolved content was not kept: %q", got)
	}
}

func TestSyncConflictContinueAcceptsResolutionCommittedOutsideKit(t *testing.T) {
	dir, _ := syncConflictRepository(t)
	service := gitservice.Service{Dir: dir}
	result, err := Sync(context.Background(), service, SyncOptions{
		Config: gitservice.DefaultWorkflowConfig(),
		Resolver: func(_ context.Context, conflict SyncConflict) (SyncConflictAction, error) {
			if conflict.ContinueErr != nil {
				t.Fatalf("manual resolution commit was not accepted: %v", conflict.ContinueErr)
			}
			if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("resolved outside kit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, dir, "add", "root.txt")
			run(t, dir, "commit", "-m", "manual conflict resolution")
			return SyncConflictContinue, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.SourceRebuilt || result.PendingKept != 1 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: %#v", result)
	}
	if got := output(t, dir, "show", "work:root.txt"); got != "resolved outside kit" {
		t.Fatalf("manual resolution commit was not kept: %q", got)
	}
}

func TestSyncConflictSkipAfterManualCommitRequiresContinueOrAbort(t *testing.T) {
	dir, _ := syncConflictRepository(t)
	service := gitservice.Service{Dir: dir}
	calls := 0
	result, err := Sync(context.Background(), service, SyncOptions{
		Config: gitservice.DefaultWorkflowConfig(),
		Resolver: func(_ context.Context, conflict SyncConflict) (SyncConflictAction, error) {
			calls++
			if calls == 1 {
				if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("resolved outside kit\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				run(t, dir, "add", "root.txt")
				run(t, dir, "commit", "-m", "manual conflict resolution")
				return SyncConflictSkip, nil
			}
			if conflict.ContinueErr == nil || !strings.Contains(conflict.ContinueErr.Error(), "already committed") {
				t.Fatalf("expected committed-resolution guidance, got %#v", conflict)
			}
			return SyncConflictContinue, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result.PendingKept != 1 || result.Skipped != 0 {
		t.Fatalf("unexpected sync result: calls=%d result=%#v", calls, result)
	}
}

func TestSyncConflictSkipReportsActualPendingKept(t *testing.T) {
	dir, _ := syncConflictRepository(t)
	service := gitservice.Service{Dir: dir}
	result, err := Sync(context.Background(), service, SyncOptions{
		Config: gitservice.DefaultWorkflowConfig(),
		Resolver: func(_ context.Context, conflict SyncConflict) (SyncConflictAction, error) {
			if len(conflict.Unresolved) == 0 {
				t.Fatalf("skip was not offered for a conflict: %#v", conflict)
			}
			return SyncConflictSkip, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.SourceRebuilt || result.PendingKept != 0 || result.Skipped != 1 {
		t.Fatalf("unexpected sync result: %#v", result)
	}
	if ok := commandOK(dir, "merge-base", "--is-ancestor", "work", "develop"); !ok {
		t.Fatal("skipped work commit remained on rebuilt work")
	}
}

func TestSyncConflictAbortKeepsOriginalWork(t *testing.T) {
	dir, _ := syncConflictRepository(t)
	originalWork := output(t, dir, "rev-parse", "work")
	service := gitservice.Service{Dir: dir}
	_, err := Sync(context.Background(), service, SyncOptions{
		Config: gitservice.DefaultWorkflowConfig(),
		Resolver: func(context.Context, SyncConflict) (SyncConflictAction, error) {
			return SyncConflictAbort, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "was aborted") {
		t.Fatalf("expected explicit abort, got %v", err)
	}
	if got := output(t, dir, "rev-parse", "work"); got != originalWork {
		t.Fatalf("work changed after abort: got %s want %s", got, originalWork)
	}
	if got := output(t, dir, "status", "--porcelain"); got != "" {
		t.Fatalf("working tree dirty after abort: %s", got)
	}
}

func TestSyncConflictContextCancellationRestoresWorkAndCleansTemporaryBranch(t *testing.T) {
	dir, _ := syncConflictRepository(t)
	originalWork := output(t, dir, "rev-parse", "work")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := gitservice.Service{Dir: dir}
	_, err := Sync(ctx, service, SyncOptions{
		Config: gitservice.DefaultWorkflowConfig(),
		Resolver: func(context.Context, SyncConflict) (SyncConflictAction, error) {
			cancel()
			return SyncConflictAbort, context.Canceled
		},
	})
	if err == nil || !strings.Contains(err.Error(), "original work was kept") {
		t.Fatalf("expected safe cancellation abort, got %v", err)
	}
	if got := output(t, dir, "rev-parse", "work"); got != originalWork {
		t.Fatalf("work changed after cancellation: got %s want %s", got, originalWork)
	}
	if got := output(t, dir, "branch", "--show-current"); got != "work" {
		t.Fatalf("checkout was not restored after cancellation: %s", got)
	}
	if got := output(t, dir, "status", "--porcelain"); got != "" {
		t.Fatalf("working tree dirty after cancellation: %s", got)
	}
	if got := output(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/kit/tmp"); got != "" {
		t.Fatalf("temporary sync branch remained after cancellation: %s", got)
	}
}

func TestSyncConflictAbortAfterExternalResolutionPreservesRecoveryBranch(t *testing.T) {
	dir, _ := syncTwoConflictRepository(t)
	originalWork := output(t, dir, "rev-parse", "work")
	service := gitservice.Service{Dir: dir}
	calls := 0
	_, err := Sync(context.Background(), service, SyncOptions{
		Config: gitservice.DefaultWorkflowConfig(),
		Resolver: func(_ context.Context, conflict SyncConflict) (SyncConflictAction, error) {
			calls++
			if calls == 1 {
				if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("resolved outside kit\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				run(t, dir, "add", "root.txt")
				run(t, dir, "commit", "-m", "manual conflict resolution")
				return SyncConflictContinue, nil
			}
			return SyncConflictAbort, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "partial rebuilt commits are preserved in kit/recovery/") {
		t.Fatalf("expected recovery branch guidance, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected two conflicts, got %d", calls)
	}
	if got := output(t, dir, "rev-parse", "work"); got != originalWork {
		t.Fatalf("work changed after abort: got %s want %s", got, originalWork)
	}
	if got := output(t, dir, "branch", "--show-current"); got != "work" {
		t.Fatalf("checkout not restored after abort: %s", got)
	}
	refs := output(t, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/kit/recovery")
	if refs == "" || strings.Contains(refs, "\n") {
		t.Fatalf("expected one recovery branch, got %q", refs)
	}
	if got := output(t, dir, "show", refs+":root.txt"); got != "resolved outside kit" {
		t.Fatalf("manual resolution was not preserved: %q", got)
	}
	if got := output(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/kit/tmp"); got != "" {
		t.Fatalf("temporary sync branch remained after recovery: %s", got)
	}
	if got := output(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads/kit/backup"); got != "" {
		t.Fatalf("recovery branch should not require retaining a verified rollback backup: %s", got)
	}
}

type failBranchDeleteRunner struct {
	gitservice.Runner
	match string
}

func (r failBranchDeleteRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if len(args) == 3 && args[0] == "branch" && args[1] == "-D" && strings.HasPrefix(args[2], r.match) {
		return nil, errors.New("injected branch deletion failure")
	}
	return r.Runner.Run(ctx, dir, args...)
}

func syncConflictRepository(t *testing.T) (string, string) {
	t.Helper()
	dir, remote := syncRepository(t)
	run(t, dir, "switch", "-c", "work")
	writeCommit(t, dir, "root.txt", "work\n", "work conflict")
	run(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "root.txt")
	run(t, dir, "commit", "-m", "team conflict")
	run(t, dir, "push", "origin", "develop")
	run(t, dir, "switch", "work")
	return dir, remote
}

func nonLinearSyncRepository(t *testing.T) (dir, baseBefore, workBefore string) {
	t.Helper()
	dir, _ = syncRepository(t)

	run(t, dir, "switch", "-c", "work")
	writeCommit(t, dir, "work.txt", "work\n", "work change")
	run(t, dir, "switch", "-c", "side")
	writeCommit(t, dir, "side.txt", "side\n", "side change")
	run(t, dir, "switch", "work")
	run(t, dir, "merge", "--no-ff", "side", "-m", "merge side into work")
	workBefore = output(t, dir, "rev-parse", "work")

	run(t, dir, "switch", "develop")
	writeCommit(t, dir, "team.txt", "team\n", "team change")
	run(t, dir, "push", "origin", "develop")
	baseBefore = output(t, dir, "rev-parse", "develop")
	run(t, dir, "switch", "work")
	return dir, baseBefore, workBefore
}

func syncTwoConflictRepository(t *testing.T) (string, string) {
	t.Helper()
	dir, remote := syncRepository(t)
	run(t, dir, "switch", "-c", "work")
	writeCommit(t, dir, "root.txt", "work root\n", "work conflict one")
	writeCommit(t, dir, "second.txt", "work second\n", "work conflict two")
	run(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("team root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second.txt"), []byte("team second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "root.txt", "second.txt")
	run(t, dir, "commit", "-m", "team conflicts")
	run(t, dir, "push", "origin", "develop")
	run(t, dir, "switch", "work")
	return dir, remote
}

func syncRepository(t *testing.T) (string, string) {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, "", "init", "--bare", remote)
	dir := t.TempDir()
	run(t, dir, "init", "-b", "develop")
	run(t, dir, "config", "user.name", "Kit Test")
	run(t, dir, "config", "user.email", "kit@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "root.txt")
	run(t, dir, "commit", "-m", "root")
	run(t, dir, "remote", "add", "origin", remote)
	run(t, dir, "push", "-u", "origin", "develop")
	return dir, remote
}

func writeCommit(t *testing.T, dir, name, content, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", name)
	run(t, dir, "commit", "-m", message)
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func output(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func commandOK(dir string, args ...string) bool {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run() == nil
}

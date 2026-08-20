package workflow

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	gitservice "kit/internal/git"
)

type SyncOptions struct {
	Config               gitservice.WorkflowConfig
	DryRun               bool
	BaseOnly             bool
	Resolver             SyncConflictResolver
	TrustedAppliedHashes map[string]struct{}
}

type SyncConflictAction string

const (
	SyncConflictContinue SyncConflictAction = "continue"
	SyncConflictSkip     SyncConflictAction = "skip"
	SyncConflictAbort    SyncConflictAction = "abort"
)

// SyncConflictResolver is called while the temporary rebuilt source branch has
// a cherry-pick conflict. It must return an explicit action; callers without a
// resolver are kept safe by aborting the rebuild.
type SyncConflictResolver func(context.Context, SyncConflict) (SyncConflictAction, error)

type SyncConflict struct {
	Commit      gitservice.Commit
	Unresolved  []string
	ContinueErr error
}

type SyncResult struct {
	Remote         string `json:"remote"`
	Base           string `json:"base"`
	Source         string `json:"source"`
	BaseBefore     string `json:"base_before"`
	BaseAfter      string `json:"base_after"`
	BaseUpdated    bool   `json:"base_updated"`
	SourceStale    bool   `json:"source_stale"`
	SourceRebuilt  bool   `json:"source_rebuilt"`
	AppliedDropped int    `json:"applied_dropped"`
	PendingKept    int    `json:"pending_kept"`
	Skipped        int    `json:"skipped"`
	BackupBranch   string `json:"backup_branch,omitempty"`
	DryRun         bool   `json:"dry_run"`
}

func Refresh(ctx context.Context, service gitservice.Service, config gitservice.WorkflowConfig, dryRun bool) (SyncResult, error) {
	result := SyncResult{Base: config.Base, Source: config.Source, DryRun: dryRun}
	clean, err := service.IsClean(ctx)
	if err != nil {
		return result, fmt.Errorf("check working tree: %w", err)
	}
	if !clean {
		return result, fmt.Errorf("working tree has changes; commit or stash them before refresh")
	}
	if err := service.VerifyRevision(ctx, config.Base); err != nil {
		return result, fmt.Errorf("base branch %q was not found: %w", config.Base, err)
	}
	if err := service.VerifyRevision(ctx, config.Source); err != nil {
		return result, fmt.Errorf("source branch %q was not found: %w", config.Source, err)
	}
	result.BaseBefore, err = service.RevisionHash(ctx, config.Base)
	if err != nil {
		return result, err
	}
	result.BaseAfter = result.BaseBefore
	synced, err := service.IsAncestor(ctx, config.Base, config.Source)
	if err != nil {
		return result, err
	}
	result.SourceStale = !synced
	commits, err := service.Candidates(ctx, config.Base, config.Source, true)
	if err != nil {
		return result, fmt.Errorf("list source commits: %w", err)
	}
	commits, err = service.Applied(ctx, config.Base, commits)
	if err != nil {
		return result, fmt.Errorf("classify source commits: %w", err)
	}
	pending := make([]gitservice.Commit, 0, len(commits))
	for _, commit := range commits {
		if commit.Applied {
			result.AppliedDropped++
		} else {
			result.PendingKept++
			pending = append(pending, commit)
		}
	}
	if dryRun || (synced && result.AppliedDropped == 0) {
		return result, nil
	}
	originalHash, originalBranch, err := service.Head(ctx)
	if err != nil {
		return result, fmt.Errorf("record current checkout: %w", err)
	}
	backup, _, err := rebuildSource(ctx, service, config.Base, config.Source, originalHash, originalBranch, "", pending, nil)
	if err != nil {
		return result, err
	}
	result.BackupBranch = backup
	result.SourceRebuilt = true
	return result, nil
}

func Sync(ctx context.Context, service gitservice.Service, opts SyncOptions) (SyncResult, error) {
	config := opts.Config
	result := SyncResult{Remote: config.Remote, Base: config.Base, Source: config.Source, DryRun: opts.DryRun}

	clean, err := service.IsClean(ctx)
	if err != nil {
		return result, fmt.Errorf("check working tree: %w", err)
	}
	if !clean {
		return result, fmt.Errorf("working tree has changes; commit or stash them before sync")
	}
	if err := service.VerifyRevision(ctx, config.Base); err != nil {
		return result, fmt.Errorf("base branch %q was not found: %w", config.Base, err)
	}
	if !opts.BaseOnly {
		if err := service.VerifyRevision(ctx, config.Source); err != nil {
			return result, fmt.Errorf("source branch %q was not found: %w", config.Source, err)
		}
	}

	result.BaseBefore, err = service.RevisionHash(ctx, config.Base)
	if err != nil {
		return result, err
	}
	if err := service.Fetch(ctx, config.Remote); err != nil {
		return result, fmt.Errorf("fetch %s: %w", config.Remote, err)
	}
	remoteBase := config.Remote + "/" + config.Base
	if err := service.VerifyRevision(ctx, remoteBase); err != nil {
		return result, fmt.Errorf("remote base %q was not found after fetch: %w", remoteBase, err)
	}
	ahead, behind, err := service.AheadBehind(ctx, config.Base, remoteBase)
	if err != nil {
		return result, fmt.Errorf("compare %s with %s: %w", config.Base, remoteBase, err)
	}
	if ahead > 0 {
		return result, fmt.Errorf("local %s has %d commit(s) not on %s; refusing a non-fast-forward sync", config.Base, ahead, remoteBase)
	}
	remoteHash, err := service.RevisionHash(ctx, remoteBase)
	if err != nil {
		return result, err
	}
	result.BaseAfter = remoteHash
	result.BaseUpdated = behind > 0

	if !opts.BaseOnly {
		synced, err := service.IsAncestor(ctx, config.Base, config.Source)
		if err != nil {
			return result, fmt.Errorf("check whether %s contains %s: %w", config.Source, config.Base, err)
		}
		result.SourceStale = !synced || behind > 0
		commits, err := service.Candidates(ctx, config.Base, config.Source, true)
		if err != nil {
			return result, fmt.Errorf("list work commits: %w", err)
		}
		commits, err = service.Applied(ctx, remoteBase, commits)
		if err != nil {
			return result, fmt.Errorf("classify work commits: %w", err)
		}
		commits = markTrustedApplied(commits, opts.TrustedAppliedHashes)
		for _, commit := range commits {
			if commit.Applied {
				result.AppliedDropped++
			} else {
				result.PendingKept++
			}
		}
	}

	if opts.DryRun {
		return result, nil
	}

	originalHash, originalBranch, err := service.Head(ctx)
	if err != nil {
		return result, fmt.Errorf("record current checkout: %w", err)
	}
	if err := updateBase(ctx, service, config.Base, remoteBase, result.BaseBefore, originalHash, originalBranch); err != nil {
		return result, err
	}
	rollbackUpdatedBase := func(cause error) error {
		recoveryCtx, cancel := recoveryContext()
		defer cancel()
		if rollbackErr := restoreBaseAndCheckout(recoveryCtx, service, config.Base, result.BaseBefore, originalHash, originalBranch); rollbackErr != nil {
			return fmt.Errorf("%v; base rollback failed: %w", cause, rollbackErr)
		}
		return fmt.Errorf("%v; base %s and original checkout were restored", cause, config.Base)
	}
	result.BaseAfter, err = service.RevisionHash(ctx, config.Base)
	if err != nil {
		return result, rollbackUpdatedBase(err)
	}
	if opts.BaseOnly {
		return result, nil
	}

	synced, err := service.IsAncestor(ctx, config.Base, config.Source)
	if err != nil {
		return result, rollbackUpdatedBase(err)
	}
	if synced && result.AppliedDropped == 0 {
		return result, nil
	}

	commits, err := service.Candidates(ctx, config.Base, config.Source, true)
	if err != nil {
		return result, rollbackUpdatedBase(fmt.Errorf("list source commits after base update: %w", err))
	}
	commits, err = service.Applied(ctx, config.Base, commits)
	if err != nil {
		return result, rollbackUpdatedBase(fmt.Errorf("classify source commits after base update: %w", err))
	}
	commits = markTrustedApplied(commits, opts.TrustedAppliedHashes)
	pending := make([]gitservice.Commit, 0, len(commits))
	result.AppliedDropped = 0
	result.PendingKept = 0
	for _, commit := range commits {
		if commit.Applied {
			result.AppliedDropped++
		} else {
			result.PendingKept++
			pending = append(pending, commit)
		}
	}
	backup, skipped, err := rebuildSource(ctx, service, config.Base, config.Source, originalHash, originalBranch, result.BaseBefore, pending, opts.Resolver)
	if err != nil {
		return result, err
	}
	result.BackupBranch = backup
	result.Skipped = skipped
	result.PendingKept = len(pending) - skipped
	result.SourceRebuilt = true
	return result, nil
}

func markTrustedApplied(commits []gitservice.Commit, trusted map[string]struct{}) []gitservice.Commit {
	if len(trusted) == 0 {
		return commits
	}
	for i := range commits {
		if _, ok := trusted[strings.ToLower(commits[i].Hash)]; ok {
			commits[i].Applied = true
		}
	}
	return commits
}

func updateBase(ctx context.Context, service gitservice.Service, base, remoteBase, baseBefore, originalHash, originalBranch string) error {
	if originalBranch != base {
		if err := service.Switch(ctx, base); err != nil {
			recoveryCtx, cancel := recoveryContext()
			defer cancel()
			if rollbackErr := restoreBaseAndCheckout(recoveryCtx, service, base, baseBefore, originalHash, originalBranch); rollbackErr != nil {
				return fmt.Errorf("switch to %s: %w; base rollback failed: %v", base, err, rollbackErr)
			}
			return fmt.Errorf("switch to %s: %w; base and original checkout were restored", base, err)
		}
	}
	if err := service.MergeFFOnly(ctx, remoteBase); err != nil {
		recoveryCtx, cancel := recoveryContext()
		defer cancel()
		if rollbackErr := restoreBaseAndCheckout(recoveryCtx, service, base, baseBefore, originalHash, originalBranch); rollbackErr != nil {
			return fmt.Errorf("fast-forward %s from %s: %w; base rollback failed: %v", base, remoteBase, err, rollbackErr)
		}
		return fmt.Errorf("fast-forward %s from %s: %w; base and original checkout were restored", base, remoteBase, err)
	}
	if originalBranch != base {
		recoveryCtx, cancel := recoveryContext()
		defer cancel()
		if err := restoreCheckout(recoveryCtx, service, originalHash, originalBranch); err != nil {
			if rollbackErr := restoreBaseAndCheckout(recoveryCtx, service, base, baseBefore, originalHash, originalBranch); rollbackErr != nil {
				return fmt.Errorf("restore checkout after updating %s: %w; base rollback failed: %v", base, err, rollbackErr)
			}
			return fmt.Errorf("restore checkout after updating %s: %w; base and original checkout were restored", base, err)
		}
	}
	return nil
}

func rebuildSource(ctx context.Context, service gitservice.Service, base, source, originalHash, originalBranch, rollbackBaseHash string, pending []gitservice.Commit, resolver SyncConflictResolver) (string, int, error) {
	sourceHash, err := service.RevisionHash(ctx, source)
	if err != nil {
		return "", 0, err
	}
	baseHash, err := service.RevisionHash(ctx, base)
	if err != nil {
		return "", 0, err
	}
	short := sourceHash
	if len(short) > 8 {
		short = short[:8]
	}
	timestamp := time.Now().UTC().Format("20060102-150405")
	backup, err := gitservice.FormatWorkBackupRef(source, gitservice.WorkBackupAuto, timestamp+"-"+short)
	if err != nil {
		return "", 0, fmt.Errorf("format backup branch: %w", err)
	}
	temporary := fmt.Sprintf("kit/tmp/%s-%d-%s", strings.ReplaceAll(source, "/", "-"), os.Getpid(), strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := service.CreateBranchAt(ctx, backup, sourceHash); err != nil {
		return "", 0, fmt.Errorf("create backup branch %s: %w", backup, err)
	}
	temporaryCreated := false
	cleanup := func() (string, bool, error) {
		recoveryCtx, cancel := recoveryContext()
		defer cancel()

		var cleanupErrors []string
		inProgress, err := service.CherryPickInProgress(recoveryCtx)
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("check cherry-pick state: %v", err))
		} else if inProgress {
			if err := service.Abort(recoveryCtx); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("abort cherry-pick: %v", err))
			}
		}

		recovery := ""
		deleteTemporary := false
		if temporaryCreated {
			temporaryHead, err := service.RevisionHash(recoveryCtx, temporary)
			if err != nil {
				recovery = temporary
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("read temporary branch %s: %v", temporary, err))
			} else if temporaryHead == baseHash {
				deleteTemporary = true
			} else {
				recovery = strings.Replace(temporary, "kit/tmp/", "kit/recovery/", 1)
				if err := service.CreateBranchAt(recoveryCtx, recovery, temporaryHead); err != nil {
					recovery = temporary
					cleanupErrors = append(cleanupErrors, fmt.Sprintf("create recovery branch %s: %v", recovery, err))
				} else {
					deleteTemporary = true
				}
			}
		}

		// A failure can happen after source or base has already moved. Detach
		// first when either ref is checked out so both refs can be restored.
		_, currentBranch, headErr := service.Head(recoveryCtx)
		if headErr != nil {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("read checkout before rollback: %v", headErr))
		} else if currentBranch == source || (rollbackBaseHash != "" && currentBranch == base) {
			detachHash := sourceHash
			if currentBranch == base {
				detachHash = rollbackBaseHash
			}
			if err := service.SwitchDetach(recoveryCtx, detachHash); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("detach %s before rollback: %v", currentBranch, err))
			}
		}
		if err := service.ForceBranch(recoveryCtx, source, sourceHash); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("restore source ref %s to %s: %v", source, sourceHash, err))
		}
		if rollbackBaseHash != "" {
			if err := service.ForceBranch(recoveryCtx, base, rollbackBaseHash); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("restore base ref %s to %s: %v", base, rollbackBaseHash, err))
			}
		}
		if err := restoreCheckout(recoveryCtx, service, originalHash, originalBranch); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("restore original checkout: %v", err))
		}

		if restoredSource, err := service.RevisionHash(recoveryCtx, source); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("verify source ref %s: %v", source, err))
		} else if restoredSource != sourceHash {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("verify source ref %s: got %s, want %s", source, restoredSource, sourceHash))
		}
		if rollbackBaseHash != "" {
			if restoredBase, err := service.RevisionHash(recoveryCtx, base); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("verify base ref %s: %v", base, err))
			} else if restoredBase != rollbackBaseHash {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("verify base ref %s: got %s, want %s", base, restoredBase, rollbackBaseHash))
			}
		}
		if restoredHash, restoredBranch, err := service.Head(recoveryCtx); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("verify original checkout: %v", err))
		} else if restoredHash != originalHash || restoredBranch != originalBranch {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("verify original checkout: got branch %q at %s, want branch %q at %s", restoredBranch, restoredHash, originalBranch, originalHash))
		}

		if len(cleanupErrors) == 0 && deleteTemporary {
			if err := service.DeleteBranch(recoveryCtx, temporary, true); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("remove temporary branch %s: %v", temporary, err))
			}
		}

		backupRemoved := false
		if len(cleanupErrors) == 0 {
			if err := service.DeleteBranch(recoveryCtx, backup, true); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("remove rollback backup %s: %v", backup, err))
			} else if exists, err := service.LocalBranchExists(recoveryCtx, backup); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("verify rollback backup removal %s: %v", backup, err))
			} else if exists {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("verify rollback backup removal %s: branch still exists", backup))
			} else {
				backupRemoved = true
			}
		}
		if len(cleanupErrors) > 0 {
			return recovery, backupRemoved, fmt.Errorf("%s", strings.Join(cleanupErrors, "; "))
		}
		return recovery, backupRemoved, nil
	}
	fail := func(commit *gitservice.Commit, cause error) error {
		recovery, backupRemoved, cleanupErr := cleanup()
		message := fmt.Sprintf("rebuild was aborted; original work was kept; source %s was restored", source)
		if commit != nil {
			message = fmt.Sprintf("reapply %s (%s) was aborted; original work was kept; source %s was restored", commit.ShortHash, commit.Subject, source)
		}
		if rollbackBaseHash != "" {
			message += "; base " + base + " was restored"
		}
		if backupRemoved {
			message += "; rollback backup " + backup + " was removed after verification"
		} else {
			message += "; rollback backup remains at " + backup
		}
		if recovery != "" {
			message += "; partial rebuilt commits are preserved in " + recovery
		}
		if cleanupErr != nil {
			message += "; cleanup issue: " + cleanupErr.Error()
		}
		if cause != nil {
			return fmt.Errorf("%s: %w", message, cause)
		}
		return fmt.Errorf("%s", message)
	}
	if err := service.CreateBranch(ctx, temporary, base); err != nil {
		return backup, 0, fail(nil, fmt.Errorf("create temporary work branch: %w", err))
	}
	temporaryCreated = true
	skipped := 0
	for _, commit := range pending {
		headBefore, err := service.RevisionHash(ctx, "HEAD")
		if err != nil {
			return backup, skipped, fail(&commit, fmt.Errorf("record temporary branch: %w", err))
		}
		if err := service.CherryPickOne(ctx, commit.Hash); err != nil {
			action, resolveErr := resolveSyncConflict(ctx, service, resolver, commit, headBefore)
			if resolveErr == nil {
				if action == SyncConflictSkip {
					skipped++
				}
				if action == SyncConflictSkip || action == SyncConflictContinue {
					continue
				}
			}
			if resolveErr != nil {
				return backup, skipped, fail(&commit, resolveErr)
			}
			return backup, skipped, fail(&commit, nil)
		}
	}
	newHead, err := service.RevisionHash(ctx, "HEAD")
	if err != nil {
		return backup, skipped, fail(nil, fmt.Errorf("read rebuilt history: %w", err))
	}
	if err := service.ForceBranch(ctx, source, newHead); err != nil {
		return backup, skipped, fail(nil, fmt.Errorf("move %s to rebuilt history: %w", source, err))
	}
	if originalBranch == source {
		recoveryCtx, cancel := recoveryContext()
		defer cancel()
		if err := service.Switch(recoveryCtx, source); err != nil {
			return backup, skipped, fail(nil, fmt.Errorf("switch to rebuilt %s: %w", source, err))
		}
	} else {
		recoveryCtx, cancel := recoveryContext()
		defer cancel()
		if err := restoreCheckout(recoveryCtx, service, originalHash, originalBranch); err != nil {
			return backup, skipped, fail(nil, fmt.Errorf("restore original checkout: %w", err))
		}
	}
	recoveryCtx, cancel := recoveryContext()
	defer cancel()
	if err := service.DeleteBranch(recoveryCtx, temporary, true); err != nil {
		return backup, skipped, fail(nil, fmt.Errorf("remove temporary branch %s: %w", temporary, err))
	}
	return backup, skipped, nil
}

func recoveryContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func resolveSyncConflict(ctx context.Context, service gitservice.Service, resolver SyncConflictResolver, commit gitservice.Commit, headBefore string) (SyncConflictAction, error) {
	if resolver == nil {
		return SyncConflictAbort, fmt.Errorf("cherry-pick conflict requires an interactive resolver; sync was safely aborted")
	}
	var continueErr error
	for {
		unresolved, err := service.Unresolved(ctx)
		if err != nil {
			return SyncConflictAbort, fmt.Errorf("check unresolved files: %w", err)
		}
		action, err := resolver(ctx, SyncConflict{Commit: commit, Unresolved: unresolved, ContinueErr: continueErr})
		if err != nil {
			return SyncConflictAbort, err
		}
		switch action {
		case SyncConflictAbort:
			return action, nil
		case SyncConflictSkip:
			inProgress, err := service.CherryPickInProgress(ctx)
			if err != nil {
				return SyncConflictAbort, fmt.Errorf("check cherry-pick state: %w", err)
			}
			if !inProgress {
				currentHead, err := service.RevisionHash(ctx, "HEAD")
				if err != nil {
					return SyncConflictAbort, fmt.Errorf("read temporary branch after external resolution: %w", err)
				}
				if currentHead != headBefore {
					continueErr = fmt.Errorf("the resolution was already committed; choose continue to keep that commit or abort to restore the original work")
					continue
				}
				return action, nil
			}
			if err := service.Skip(ctx); err != nil {
				continueErr = fmt.Errorf("skip cherry-pick: %w", err)
				continue
			}
			return action, nil
		case SyncConflictContinue:
			unresolved, err = service.Unresolved(ctx)
			if err != nil {
				return SyncConflictAbort, fmt.Errorf("check unresolved files: %w", err)
			}
			if len(unresolved) > 0 {
				continueErr = fmt.Errorf("unresolved files remain: %s", strings.Join(unresolved, ", "))
				continue
			}
			inProgress, err := service.CherryPickInProgress(ctx)
			if err != nil {
				return SyncConflictAbort, fmt.Errorf("check cherry-pick state: %w", err)
			}
			if !inProgress {
				currentHead, err := service.RevisionHash(ctx, "HEAD")
				if err != nil {
					return SyncConflictAbort, fmt.Errorf("read temporary branch after external resolution: %w", err)
				}
				if currentHead != headBefore {
					return action, nil
				}
				continueErr = fmt.Errorf("cherry-pick state is missing and no resolution commit was detected; choose skip or abort")
				continue
			}
			if err := service.Continue(ctx); err != nil {
				continueErr = fmt.Errorf("continue cherry-pick: %w", err)
				continue
			}
			return action, nil
		default:
			continueErr = fmt.Errorf("choose continue, skip, or abort")
		}
	}
}

func restoreCheckout(ctx context.Context, service gitservice.Service, hash, branch string) error {
	if branch != "" {
		return service.Switch(ctx, branch)
	}
	return service.SwitchDetach(ctx, hash)
}

func restoreBaseAndCheckout(ctx context.Context, service gitservice.Service, base, baseHash, originalHash, originalBranch string) error {
	var rollbackErrors []string
	_, currentBranch, err := service.Head(ctx)
	if err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("read checkout: %v", err))
	} else if currentBranch == base {
		if err := service.SwitchDetach(ctx, baseHash); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Sprintf("detach %s: %v", base, err))
		}
	}
	if err := service.ForceBranch(ctx, base, baseHash); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("restore base ref %s: %v", base, err))
	}
	if err := restoreCheckout(ctx, service, originalHash, originalBranch); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("restore original checkout: %v", err))
	}
	if restoredBase, err := service.RevisionHash(ctx, base); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("verify base ref %s: %v", base, err))
	} else if restoredBase != baseHash {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("verify base ref %s: got %s, want %s", base, restoredBase, baseHash))
	}
	if restoredHash, restoredBranch, err := service.Head(ctx); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("verify original checkout: %v", err))
	} else if restoredHash != originalHash || restoredBranch != originalBranch {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("verify original checkout: got branch %q at %s, want branch %q at %s", restoredBranch, restoredHash, originalBranch, originalHash))
	}
	if len(rollbackErrors) > 0 {
		return fmt.Errorf("%s", strings.Join(rollbackErrors, "; "))
	}
	return nil
}

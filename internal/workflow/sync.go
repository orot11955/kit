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
	backup, _, err := rebuildSource(ctx, service, config.Base, config.Source, originalHash, originalBranch, pending, nil)
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
	if err := updateBase(ctx, service, config.Base, remoteBase, originalHash, originalBranch); err != nil {
		return result, err
	}
	result.BaseAfter, err = service.RevisionHash(ctx, config.Base)
	if err != nil {
		return result, err
	}
	if opts.BaseOnly {
		return result, nil
	}

	synced, err := service.IsAncestor(ctx, config.Base, config.Source)
	if err != nil {
		return result, err
	}
	if synced && result.AppliedDropped == 0 {
		return result, nil
	}

	commits, err := service.Candidates(ctx, config.Base, config.Source, true)
	if err != nil {
		return result, fmt.Errorf("list source commits after base update: %w", err)
	}
	commits, err = service.Applied(ctx, config.Base, commits)
	if err != nil {
		return result, fmt.Errorf("classify source commits after base update: %w", err)
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
	backup, skipped, err := rebuildSource(ctx, service, config.Base, config.Source, originalHash, originalBranch, pending, opts.Resolver)
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

func updateBase(ctx context.Context, service gitservice.Service, base, remoteBase, originalHash, originalBranch string) error {
	if originalBranch != base {
		if err := service.Switch(ctx, base); err != nil {
			recoveryCtx, cancel := recoveryContext()
			defer cancel()
			_ = restoreCheckout(recoveryCtx, service, originalHash, originalBranch)
			return fmt.Errorf("switch to %s: %w", base, err)
		}
	}
	if err := service.MergeFFOnly(ctx, remoteBase); err != nil {
		recoveryCtx, cancel := recoveryContext()
		defer cancel()
		_ = restoreCheckout(recoveryCtx, service, originalHash, originalBranch)
		return fmt.Errorf("fast-forward %s from %s: %w", base, remoteBase, err)
	}
	if originalBranch != base {
		recoveryCtx, cancel := recoveryContext()
		defer cancel()
		if err := restoreCheckout(recoveryCtx, service, originalHash, originalBranch); err != nil {
			return fmt.Errorf("restore checkout after updating %s: %w", base, err)
		}
	}
	return nil
}

func rebuildSource(ctx context.Context, service gitservice.Service, base, source, originalHash, originalBranch string, pending []gitservice.Commit, resolver SyncConflictResolver) (string, int, error) {
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
	backup := fmt.Sprintf("kit/backup/%s-%s-%s", strings.ReplaceAll(source, "/", "-"), timestamp, short)
	temporary := fmt.Sprintf("kit/tmp/%s-%d-%s", strings.ReplaceAll(source, "/", "-"), os.Getpid(), strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := service.CreateBranchAt(ctx, backup, sourceHash); err != nil {
		return "", 0, fmt.Errorf("create backup branch %s: %w", backup, err)
	}
	if err := service.CreateBranch(ctx, temporary, base); err != nil {
		return backup, 0, fmt.Errorf("create temporary work branch: %w", err)
	}
	cleanup := func() (string, error) {
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
		temporaryHead, err := service.RevisionHash(recoveryCtx, temporary)
		if err != nil {
			recovery = temporary
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("read temporary branch: %v", err))
		} else if temporaryHead == baseHash {
			deleteTemporary = true
		} else {
			recovery = strings.Replace(temporary, "kit/tmp/", "kit/recovery/", 1)
			if err := service.CreateBranchAt(recoveryCtx, recovery, temporaryHead); err != nil {
				recovery = temporary
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("create recovery branch: %v", err))
			} else {
				deleteTemporary = true
			}
		}

		restored := true
		if err := restoreCheckout(recoveryCtx, service, originalHash, originalBranch); err != nil {
			restored = false
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("restore original checkout: %v", err))
		}
		if restored && deleteTemporary {
			if err := service.DeleteBranch(recoveryCtx, temporary, true); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Sprintf("remove temporary branch: %v", err))
			}
		}
		if len(cleanupErrors) > 0 {
			return recovery, fmt.Errorf("%s", strings.Join(cleanupErrors, "; "))
		}
		return recovery, nil
	}
	fail := func(commit *gitservice.Commit, cause error) error {
		recovery, cleanupErr := cleanup()
		message := fmt.Sprintf("rebuild was aborted; original %s was kept and backup is %s", source, backup)
		if commit != nil {
			message = fmt.Sprintf("reapply %s (%s) was aborted; original %s was kept and backup is %s", commit.ShortHash, commit.Subject, source, backup)
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
		recoveryCtx, cancel := recoveryContext()
		_ = service.ForceBranch(recoveryCtx, source, sourceHash)
		cancel()
		return backup, skipped, fail(nil, fmt.Errorf("move %s to rebuilt history: %w", source, err))
	}
	if originalBranch == source {
		recoveryCtx, cancel := recoveryContext()
		defer cancel()
		if err := service.Switch(recoveryCtx, source); err != nil {
			_ = service.ForceBranch(recoveryCtx, source, sourceHash)
			cancel()
			return backup, skipped, fail(nil, fmt.Errorf("switch to rebuilt %s: %w", source, err))
		}
	} else {
		recoveryCtx, cancel := recoveryContext()
		defer cancel()
		if err := restoreCheckout(recoveryCtx, service, originalHash, originalBranch); err != nil {
			_ = service.ForceBranch(recoveryCtx, source, sourceHash)
			cancel()
			return backup, skipped, fail(nil, fmt.Errorf("restore original checkout: %w", err))
		}
	}
	recoveryCtx, cancel := recoveryContext()
	defer cancel()
	if err := service.DeleteBranch(recoveryCtx, temporary, true); err != nil {
		return backup, skipped, fmt.Errorf("remove temporary branch %s: %w", temporary, err)
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

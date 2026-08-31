package app

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	gitservice "kit/internal/git"
	"kit/internal/reviewstate"
)

type reviewReconcileResult struct {
	Dropped      int
	BackupBranch string
}

// reconcileMergedReviewQueue is provider-aware cleanup used only after a PR is
// confirmed merged. Normal `kit sync` deliberately remains Git-only.
func reconcileMergedReviewQueue(ctx context.Context, service gitservice.Service, config gitservice.WorkflowConfig, state reviewstate.State) (reviewReconcileResult, error) {
	result := reviewReconcileResult{}
	if len(state.SourceCommits) == 0 {
		return result, nil
	}
	trusted := make(map[string]struct{}, len(state.SourceCommits))
	for _, hash := range state.SourceCommits {
		hash = strings.ToLower(strings.TrimSpace(hash))
		if len(hash) == 40 {
			trusted[hash] = struct{}{}
		}
	}
	if len(trusted) == 0 {
		return result, nil
	}
	candidates, err := service.Candidates(ctx, config.Base, config.Source, true)
	if err != nil {
		return result, fmt.Errorf("list source commits for review reconcile: %w", err)
	}
	keep := make([]gitservice.Commit, 0, len(candidates))
	for _, commit := range candidates {
		drop := false
		if _, ok := trusted[strings.ToLower(commit.Hash)]; ok {
			drop = true
		} else if original, ok, sourceErr := service.CherryPickedFrom(ctx, commit.Hash); sourceErr != nil {
			return result, fmt.Errorf("inspect source commit %s: %w", commit.ShortHash, sourceErr)
		} else if ok {
			_, drop = trusted[strings.ToLower(original)]
		}
		if drop {
			result.Dropped++
			continue
		}
		keep = append(keep, commit)
	}
	if result.Dropped == 0 {
		return result, nil
	}
	clean, err := service.IsClean(ctx)
	if err != nil {
		return result, fmt.Errorf("check working tree before review reconcile: %w", err)
	}
	if !clean {
		return result, fmt.Errorf("working tree changed after sync; refusing review reconcile")
	}

	sourceHash, err := service.RevisionHash(ctx, config.Source)
	if err != nil {
		return result, err
	}
	originalHash, originalBranch, err := service.Head(ctx)
	if err != nil {
		return result, fmt.Errorf("record checkout before review reconcile: %w", err)
	}
	short := sourceHash
	if len(short) > 8 {
		short = short[:8]
	}
	id := time.Now().UTC().Format("20060102-150405") + "-" + short
	backup, err := gitservice.FormatWorkBackupRef(config.Source, gitservice.WorkBackupAuto, id)
	if err != nil {
		return result, fmt.Errorf("format review reconcile backup: %w", err)
	}
	if err := service.CreateBranchAt(ctx, backup, sourceHash); err != nil {
		return result, fmt.Errorf("create review reconcile backup %s: %w", backup, err)
	}
	result.BackupBranch = backup
	temporary := fmt.Sprintf("kit/tmp/reconcile-%s-%d-%s", strings.ReplaceAll(config.Source, "/", "-"), os.Getpid(), strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := service.CreateBranch(ctx, temporary, config.Base); err != nil {
		return result, fmt.Errorf("create review reconcile branch: %w; original source remains in %s", err, backup)
	}

	fail := func(cause error) error {
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if inProgress, progressErr := service.CherryPickInProgress(recoveryCtx); progressErr == nil && inProgress {
			_ = service.Abort(recoveryCtx)
		}
		_ = restoreReconcileCheckout(recoveryCtx, service, originalHash, originalBranch)
		_ = service.DeleteBranch(recoveryCtx, temporary, true)
		return fmt.Errorf("review reconcile aborted; source %s was not moved and backup remains at %s: %w", config.Source, backup, cause)
	}

	for _, commit := range keep {
		if err := service.CherryPickOne(ctx, commit.Hash); err != nil {
			return result, fail(fmt.Errorf("reapply %s (%s): %w", commit.ShortHash, commit.Subject, err))
		}
	}
	newHead, err := service.RevisionHash(ctx, "HEAD")
	if err != nil {
		return result, fail(fmt.Errorf("read reconciled source: %w", err))
	}
	if err := service.ForceBranch(ctx, config.Source, newHead); err != nil {
		return result, fail(fmt.Errorf("move %s to reconciled history: %w", config.Source, err))
	}
	if err := restoreReconcileCheckout(ctx, service, originalHash, originalBranch); err != nil {
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = service.ForceBranch(recoveryCtx, config.Source, sourceHash)
		_ = restoreReconcileCheckout(recoveryCtx, service, originalHash, originalBranch)
		return result, fmt.Errorf("review reconcile checkout restore failed; source rollback was attempted and backup remains at %s: %w", backup, err)
	}
	if err := service.DeleteBranch(ctx, temporary, true); err != nil {
		return result, fmt.Errorf("review reconcile succeeded but temporary branch %s could not be removed: %w", temporary, err)
	}
	return result, nil
}

func restoreReconcileCheckout(ctx context.Context, service gitservice.Service, hash, branch string) error {
	if branch != "" {
		return service.Switch(ctx, branch)
	}
	return service.SwitchDetach(ctx, hash)
}

func cleanupFinishedReviewBranch(ctx context.Context, service gitservice.Service, config gitservice.WorkflowConfig, state reviewstate.State, managed bool) (localRemoved, remoteRemoved bool, err error) {
	if !managed || state.PublishedTip == "" {
		return false, false, nil
	}
	localExists, err := service.LocalBranchExists(ctx, state.Branch)
	if err != nil {
		return false, false, err
	}
	if localExists {
		tip, tipErr := service.RevisionHash(ctx, state.Branch)
		if tipErr != nil {
			return false, false, tipErr
		}
		if !strings.EqualFold(tip, state.PublishedTip) {
			return false, false, fmt.Errorf("refusing finished review cleanup: local %s moved from published tip %s to %s", state.Branch, state.PublishedTip, tip)
		}
		_, current, headErr := service.Head(ctx)
		if headErr != nil {
			return false, false, headErr
		}
		if current == state.Branch {
			if switchErr := service.Switch(ctx, config.Source); switchErr != nil {
				return false, false, fmt.Errorf("switch to %s before review cleanup: %w", config.Source, switchErr)
			}
		}
		if deleteErr := service.DeleteBranch(ctx, state.Branch, true); deleteErr != nil {
			return false, false, fmt.Errorf("remove merged local review branch %s: %w", state.Branch, deleteErr)
		}
		_ = service.ClearKitCreatedBranch(ctx, state.Branch)
		localRemoved = true
	}
	remoteRemoved, err = service.DeleteRemoteBranchIfMatches(ctx, config.Remote, state.Branch, state.PublishedTip)
	if err != nil {
		return localRemoved, false, err
	}
	return localRemoved, remoteRemoved, nil
}

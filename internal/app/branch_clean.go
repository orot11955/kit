package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
)

type branchCleanEntry struct {
	Branch  string `json:"branch"`
	Reason  string `json:"reason"`
	Commits int    `json:"commits,omitempty"`
}

type branchCleanResult struct {
	Base         string             `json:"base"`
	DryRun       bool               `json:"dry_run"`
	Candidates   []branchCleanEntry `json:"candidates"`
	Protected    []branchCleanEntry `json:"protected"`
	Retained     []branchCleanEntry `json:"retained"`
	Deleted      []string           `json:"deleted"`
	ClearedMarks []string           `json:"cleared_markers"`
}

func (a *Application) branchCleanCommand(ctx context.Context, global globalOptions, args []string) error {
	deleteBranches := false
	for len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			printBranchCleanHelp(a.IO.Out)
			return nil
		}
		if consumed, err := parseGlobal(&global, args); err != nil {
			return err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		switch args[0] {
		case "--delete":
			deleteBranches, args = true, args[1:]
		default:
			return clierror.New(clierror.Usage, "unknown branch-clean option %q", args[0])
		}
	}
	if deleteBranches && global.json && !global.yes {
		return clierror.New(clierror.Usage, "branch-clean --delete --json requires --yes")
	}
	service, err := a.validatedGit(ctx, global.cwd)
	if err != nil {
		return err
	}
	result, err := inspectBranchCleanup(ctx, service)
	if err != nil {
		return err
	}
	result.DryRun = !deleteBranches
	if !deleteBranches {
		return a.printBranchClean(global, result)
	}
	if len(result.Candidates) == 0 && len(result.ClearedMarks) == 0 {
		return a.printBranchClean(global, result)
	}
	if !global.yes {
		ok, err := confirm(a.IO.In, a.IO.Out, fmt.Sprintf("안전 판정된 Kit branch %d개를 삭제하고 stale marker를 정리하시겠습니까? remote branch는 건드리지 않습니다. [y/N] ", len(result.Candidates)))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}
	for _, entry := range result.Candidates {
		if err := service.DeleteBranch(ctx, entry.Branch, true); err != nil {
			return clierror.Wrap(clierror.Failure, err, "delete safe Kit branch %q", entry.Branch)
		}
		if err := service.ClearKitCreatedBranch(ctx, entry.Branch); err != nil {
			return clierror.Wrap(clierror.Failure, err, "clear Kit branch marker %q after deletion", entry.Branch)
		}
		result.Deleted = append(result.Deleted, entry.Branch)
	}
	for _, branch := range result.ClearedMarks {
		if err := service.ClearKitCreatedBranch(ctx, branch); err != nil {
			return clierror.Wrap(clierror.Failure, err, "clear stale Kit branch marker %q", branch)
		}
	}
	result.DryRun = false
	return a.printBranchClean(global, result)
}

func inspectBranchCleanup(ctx context.Context, service gitservice.Service) (branchCleanResult, error) {
	config := service.WorkflowConfig(ctx)
	result := branchCleanResult{
		Base:         config.Base,
		Candidates:   []branchCleanEntry{},
		Protected:    []branchCleanEntry{},
		Retained:     []branchCleanEntry{},
		Deleted:      []string{},
		ClearedMarks: []string{},
	}
	marked, err := service.KitCreatedBranches(ctx)
	if err != nil {
		return result, clierror.Wrap(clierror.Failure, err, "list Kit-created branches")
	}
	worktrees, err := service.Worktrees(ctx)
	if err != nil {
		return result, clierror.Wrap(clierror.Failure, err, "list worktrees before branch cleanup")
	}
	inUse := make(map[string]struct{}, len(worktrees))
	for _, item := range worktrees {
		if item.Branch != "" {
			inUse[item.Branch] = struct{}{}
		}
	}
	_, current, err := service.Head(ctx)
	if err != nil {
		return result, clierror.Wrap(clierror.Failure, err, "read current branch")
	}

	for _, branch := range marked {
		exists, err := service.LocalBranchExists(ctx, branch)
		if err != nil {
			return result, clierror.Wrap(clierror.Failure, err, "check Kit branch %q", branch)
		}
		if !exists {
			result.ClearedMarks = append(result.ClearedMarks, branch)
			continue
		}
		if reason := protectedCleanupReason(branch, current, config, inUse); reason != "" {
			result.Protected = append(result.Protected, branchCleanEntry{Branch: branch, Reason: reason})
			continue
		}
		ancestor, err := service.IsAncestor(ctx, branch, config.Base)
		if err != nil {
			return result, clierror.Wrap(clierror.Failure, err, "check whether %s is merged into %s", branch, config.Base)
		}
		if ancestor {
			result.Candidates = append(result.Candidates, branchCleanEntry{Branch: branch, Reason: "branch tip is an ancestor of base"})
			continue
		}
		mergeCount, err := service.MergeCount(ctx, config.Base, branch)
		if err != nil {
			return result, clierror.Wrap(clierror.Failure, err, "inspect merge commits in %s", branch)
		}
		if mergeCount > 0 {
			result.Retained = append(result.Retained, branchCleanEntry{Branch: branch, Reason: "contains review-side merge commits not represented by the safe first-parent patch check", Commits: mergeCount})
			continue
		}
		commits, err := service.Candidates(ctx, config.Base, branch, true)
		if err != nil {
			return result, clierror.Wrap(clierror.Failure, err, "list commits from Kit branch %s", branch)
		}
		if len(commits) == 0 {
			result.Retained = append(result.Retained, branchCleanEntry{Branch: branch, Reason: "branch relationship is not safely classifiable"})
			continue
		}
		commits, err = service.Applied(ctx, config.Base, commits)
		if err != nil {
			return result, clierror.Wrap(clierror.Failure, err, "compare Kit branch patches against %s", config.Base)
		}
		allApplied := true
		for _, commit := range commits {
			if !commit.Applied {
				allApplied = false
				break
			}
		}
		if allApplied {
			result.Candidates = append(result.Candidates, branchCleanEntry{Branch: branch, Reason: "all branch commits are already applied to base", Commits: len(commits)})
		} else {
			result.Retained = append(result.Retained, branchCleanEntry{Branch: branch, Reason: "contains commits that are not applied to base", Commits: len(commits)})
		}
	}
	return result, nil
}

func protectedCleanupReason(branch, current string, config gitservice.WorkflowConfig, inUse map[string]struct{}) string {
	if branch == config.Stable || branch == config.Base || branch == config.Source {
		return "protected workflow branch"
	}
	if branch == current {
		return "current checkout"
	}
	if _, ok := inUse[branch]; ok {
		return "checked out by a worktree"
	}
	for _, prefix := range []string{"kit/backup/", "kit/recovery/", "kit/tmp/"} {
		if strings.HasPrefix(branch, prefix) {
			return "internal recovery namespace"
		}
	}
	return ""
}

func (a *Application) printBranchClean(global globalOptions, result branchCleanResult) error {
	if global.json {
		return writeJSON(a.IO.Out, result)
	}
	r := a.renderer(global)
	r.Command("branch-clean")
	r.Field("Base", result.Base)
	if result.DryRun {
		r.Notice("dry-run")
	}
	for _, entry := range result.Candidates {
		r.Success("Candidate", fmt.Sprintf("%s · %s", entry.Branch, entry.Reason))
	}
	for _, entry := range result.Protected {
		r.Warning("Protected", fmt.Sprintf("%s · %s", entry.Branch, entry.Reason))
	}
	for _, entry := range result.Retained {
		r.Pending("Retained", fmt.Sprintf("%s · %s", entry.Branch, entry.Reason))
	}
	for _, branch := range result.ClearedMarks {
		if result.DryRun {
			r.Field("Stale marker", branch)
		} else {
			r.Success("Marker", branch+" 정리")
		}
	}
	for _, branch := range result.Deleted {
		r.Success("Deleted", branch)
	}
	if result.DryRun && (len(result.Candidates) > 0 || len(result.ClearedMarks) > 0) {
		r.Next("kit branch-clean --delete")
	}
	return nil
}

func printBranchCleanHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: kit branch-clean [--delete] [--json] [--yes]

By default this is a dry-run. Only local branches carrying Kit's review-branch
marker are considered. main/develop/work, current checkout, branches used by any
worktree, internal recovery namespaces, and branches with unapplied work are
preserved. Remote branches are never deleted.

Options:
  --delete       Delete only branches classified safe and clear stale markers
  --cwd <path>   Git repository directory
  --json         Print machine-readable output
  --yes          Skip deletion confirmation
`)
}

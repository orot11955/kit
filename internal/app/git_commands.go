package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
	"kit/internal/pickstate"
	"kit/internal/review"
	"kit/internal/reviewstate"
	"kit/internal/ui"
	"kit/internal/workflow"
)

type gitStatusResult struct {
	Config         gitservice.WorkflowConfig `json:"config"`
	CurrentBranch  string                    `json:"current_branch"`
	Clean          bool                      `json:"clean"`
	SourceSynced   bool                      `json:"source_synced"`
	MergeCommits   int                       `json:"merge_commits"`
	Applied        int                       `json:"applied"`
	Pending        int                       `json:"pending"`
	RemoteURL      string                    `json:"remote_url,omitempty"`
	Provider       string                    `json:"provider"`
	BaseAhead      int                       `json:"base_ahead"`
	BaseBehind     int                       `json:"base_behind"`
	RemoteObserved bool                      `json:"remote_observed"`
	Operation      string                    `json:"operation"`
	Reviews        []reviewstate.State       `json:"reviews,omitempty"`
	ReviewWarnings []string                  `json:"review_warnings,omitempty"`
}

const statusReviewRefreshTimeout = 5 * time.Second

func (a *Application) statusRefreshTimeout() time.Duration {
	if a.statusReviewRefreshTimeout > 0 {
		return a.statusReviewRefreshTimeout
	}
	return statusReviewRefreshTimeout
}

func (a *Application) gitStatus(ctx context.Context, global globalOptions, args []string) error {
	return a.gitStatusWithReviews(ctx, global, args, false)
}

func (a *Application) statusCommand(ctx context.Context, global globalOptions, args []string) error {
	return a.gitStatusWithReviews(ctx, global, args, true)
}

func (a *Application) gitStatusWithReviews(ctx context.Context, global globalOptions, args []string, refreshReviews bool) error {
	fetch := false
	for len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Fprintln(a.IO.Out, "Usage: kit status [--fetch] [--cached] [--json] [--cwd <path>]")
			return nil
		}
		if consumed, err := parseGlobal(&global, args); err != nil {
			return err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		if args[0] == "--fetch" {
			fetch, args = true, args[1:]
			continue
		}
		if args[0] == "--cached" {
			refreshReviews, args = false, args[1:]
			continue
		}
		return clierror.New(clierror.Usage, "unknown git status option %q", args[0])
	}
	service, err := a.validatedGit(ctx, global.cwd)
	if err != nil {
		return err
	}
	config := service.WorkflowConfig(ctx)
	if fetch {
		if err := service.Fetch(ctx, config.Remote); err != nil {
			return clierror.Wrap(clierror.Failure, err, "fetch %s", config.Remote)
		}
	}
	if err := verifyRevisions(ctx, service, config.Base, config.Source); err != nil {
		return err
	}
	_, branch, err := service.Head(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "read current branch")
	}
	clean, err := service.IsClean(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check working tree")
	}
	synced, err := service.IsAncestor(ctx, config.Base, config.Source)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check work synchronization")
	}
	merges, err := service.MergeCount(ctx, config.Base, config.Source)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "count source merges")
	}
	commits, err := service.Candidates(ctx, config.Base, config.Source, false)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "list source commits")
	}
	commits, err = service.Applied(ctx, config.Base, commits)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "classify source commits")
	}
	result := gitStatusResult{Config: config, CurrentBranch: branch, Clean: clean, SourceSynced: synced, MergeCommits: merges}
	if state, stateErr := pickstate.Load(ctx, service); stateErr == nil {
		result.Operation = fmt.Sprintf("pick %s (%d/%d)", state.TargetBranch, state.Next, len(state.Commits))
	} else {
		result.Operation = "none"
	}
	states, err := reviewstate.List(ctx, service)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "list tracked reviews")
	}
	reviewCtx := withInsecureHTTPWarningOnce(ctx)
	if refreshReviews {
		var cancel context.CancelFunc
		reviewCtx, cancel = context.WithTimeout(reviewCtx, a.statusRefreshTimeout())
		defer cancel()
	}
	for _, state := range states {
		if state.Stage != reviewstate.StageCleaned {
			if refreshReviews && state.Stage != reviewstate.StagePicked && state.Stage != reviewstate.StageClosed {
				refreshed, _, refreshErr := a.refreshReview(reviewCtx, service, state.Branch)
				if refreshErr != nil {
					result.ReviewWarnings = append(result.ReviewWarnings, fmt.Sprintf("%s: %v", state.Branch, refreshErr))
				} else {
					state = refreshed
				}
			}
			result.Reviews = append(result.Reviews, state)
		}
	}
	for _, commit := range commits {
		if commit.Applied {
			result.Applied++
		} else {
			result.Pending++
		}
	}
	if remoteURL, remoteErr := service.RemoteURL(ctx, config.Remote); remoteErr == nil {
		repository := hosting.Resolve(config.Provider, remoteURL)
		result.RemoteURL = repository.Remote
		result.Provider = repository.Provider
	} else {
		result.Provider = hosting.Resolve(config.Provider, "").Provider
	}
	remoteBase := config.Remote + "/" + config.Base
	if err := service.VerifyRevision(ctx, remoteBase); err == nil {
		result.BaseAhead, result.BaseBehind, err = service.AheadBehind(ctx, config.Base, remoteBase)
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "compare local and remote base")
		}
		result.RemoteObserved = true
	}
	if global.json {
		return writeJSON(a.IO.Out, result)
	}
	renderer := a.renderer(global)
	renderer.Command("status")
	renderer.Section("저장소")
	renderer.Field("Provider", result.Provider)
	renderer.Field("Remote", config.Remote)
	renderer.Field("Branch", branch)
	if clean {
		renderer.Success("Tree", "clean")
	} else {
		renderer.Warning("Tree", "커밋되지 않은 변경이 있습니다.")
	}
	renderer.Section("워크플로")
	if result.RemoteObserved {
		baseState := fmt.Sprintf("%s · %s보다 ahead %d / behind %d", config.Base, config.Remote, result.BaseAhead, result.BaseBehind)
		if result.BaseAhead > 0 || result.BaseBehind > 0 {
			renderer.Warning("Base", baseState)
		} else {
			renderer.Success("Base", baseState)
		}
	} else {
		renderer.Field("Base", config.Base+" · 원격 상태 미확인")
	}
	if !result.SourceSynced {
		renderer.Warning("Work", fmt.Sprintf("%s · 현재 base 미포함 · %d개 대기", config.Source, result.Pending))
	} else if result.Pending > 0 {
		renderer.Pending("Work", fmt.Sprintf("%s · %d개 미반영 커밋", config.Source, result.Pending))
	} else {
		renderer.Success("Work", config.Source+" · 대기 작업 없음")
	}
	if result.Operation != "none" {
		renderer.Warning("Paused", result.Operation)
	}
	renderer.Section("리뷰")
	hasMergedReview := false
	if len(result.Reviews) == 0 {
		renderer.Field("State", "진행 중인 리뷰 없음")
	}
	for _, state := range result.Reviews {
		value := fmt.Sprintf("%s → %s · %s", state.Branch, state.TargetBranch, reviewStageLabel(state.Stage))
		if state.ReviewNumber > 0 {
			value = fmt.Sprintf("%s → %s · #%d · %s", state.Branch, state.TargetBranch, state.ReviewNumber, reviewStageLabel(state.Stage))
		}
		switch state.Stage {
		case reviewstate.StageMerged, reviewstate.StageSynced:
			hasMergedReview = true
			renderer.Success("Review", value)
		case reviewstate.StageClosed:
			renderer.Warning("Review", value)
		default:
			renderer.Pending("Review", value)
		}
	}
	for _, warning := range result.ReviewWarnings {
		renderer.Warning("Refresh", warning+" · cached 상태 표시")
	}
	if result.Operation != "none" {
		renderer.Next("kit pick --continue")
	} else if hasMergedReview || !result.SourceSynced || result.BaseBehind > 0 {
		renderer.Next("kit sync")
	} else if result.BaseAhead > 0 {
		renderer.Next("git log --oneline " + ui.ShellQuote(config.Remote+"/"+config.Base+".."+config.Base))
	}
	return nil
}

func reviewStageLabel(stage reviewstate.Stage) string {
	switch stage {
	case reviewstate.StagePicked:
		return "로컬 준비"
	case reviewstate.StagePublished:
		return "push 완료"
	case reviewstate.StageOpen:
		return "검토 중"
	case reviewstate.StageMerged:
		return "머지 완료"
	case reviewstate.StageSynced:
		return "동기화 완료"
	case reviewstate.StageCleaned:
		return "정리 완료"
	case reviewstate.StageClosed:
		return "머지 없이 종료"
	default:
		return string(stage)
	}
}

type syncOptions struct {
	dryRun   bool
	baseOnly bool
}

func (a *Application) gitSync(ctx context.Context, global globalOptions, args []string) error {
	opts, global, help, err := parseSyncOptions(global, args)
	if err != nil {
		return err
	}
	if help {
		printSyncHelp(a.IO.Out)
		return nil
	}
	return a.gitSyncWithOptions(ctx, global, opts)
}

func (a *Application) syncCommand(ctx context.Context, global globalOptions, args []string) error {
	opts, global, help, err := parseSyncOptions(global, args)
	if err != nil {
		return err
	}
	if help {
		printSyncHelp(a.IO.Out)
		return nil
	}
	if global.json && !global.yes && !opts.dryRun {
		return clierror.New(clierror.Usage, "sync --json requires --yes or --dry-run")
	}
	if opts.dryRun && !opts.baseOnly {
		service, err := a.validatedGit(ctx, global.cwd)
		if err != nil {
			return err
		}
		handled, err := a.previewMergedReviewBeforeSync(ctx, global, service)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
	}
	if !opts.dryRun && !opts.baseOnly {
		service, err := a.validatedGit(ctx, global.cwd)
		if err != nil {
			return err
		}
		handled, err := a.finishMergedReviewBeforeSync(ctx, global, service)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
	}
	return a.gitSyncWithOptions(ctx, global, opts)
}

func parseSyncOptions(global globalOptions, args []string) (syncOptions, globalOptions, bool, error) {
	opts := syncOptions{}
	for len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			return opts, global, true, nil
		}
		if consumed, err := parseGlobal(&global, args); err != nil {
			return opts, global, false, err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		switch args[0] {
		case "--dry-run":
			opts.dryRun, args = true, args[1:]
		case "--base-only":
			opts.baseOnly, args = true, args[1:]
		default:
			return opts, global, false, clierror.New(clierror.Usage, "unknown sync option %q", args[0])
		}
	}
	return opts, global, false, nil
}

func printSyncHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage: kit sync [--dry-run] [--base-only] [--yes] [--json] [--cwd <path>]")
}

func (a *Application) gitSyncWithOptions(ctx context.Context, global globalOptions, opts syncOptions) error {
	if global.json && !global.yes && !opts.dryRun {
		return clierror.New(clierror.Usage, "sync --json requires --yes or --dry-run")
	}
	service, err := a.validatedGit(ctx, global.cwd)
	if err != nil {
		return err
	}
	config := service.WorkflowConfig(ctx)
	plan, err := workflow.Sync(ctx, service, workflow.SyncOptions{Config: config, DryRun: true, BaseOnly: opts.baseOnly})
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "plan sync")
	}
	if opts.dryRun {
		if global.json {
			return writeJSON(a.IO.Out, plan)
		}
		printSyncResult(a.renderer(global), plan)
		return nil
	}
	reader := bufio.NewReader(a.IO.In)
	if !global.yes {
		printSyncResult(a.renderer(global), plan)
		ok, err := confirmReaderContext(ctx, reader, a.IO.Out, "이 계획으로 동기화하시겠습니까? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}
	var resolver workflow.SyncConflictResolver
	if !global.json {
		resolver = func(ctx context.Context, conflict workflow.SyncConflict) (workflow.SyncConflictAction, error) {
			return a.resolveSyncConflict(ctx, reader, conflict)
		}
	}
	// Confirmation deliberately uses the caller's context. Once mutation begins,
	// capture Ctrl-C here so cancellation enters workflow's rollback path instead
	// of relying on the process-wide default signal action.
	mutationCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := workflow.Sync(mutationCtx, service, workflow.SyncOptions{Config: config, BaseOnly: opts.baseOnly, Resolver: resolver})
	if commandErr := syncCommandError(mutationCtx, err); commandErr != nil {
		return commandErr
	}
	if global.json {
		return writeJSON(a.IO.Out, result)
	}
	printSyncResult(a.renderer(global), result)
	return nil
}

func (a *Application) finishMergedReviewBeforeSync(ctx context.Context, global globalOptions, service gitservice.Service) (bool, error) {
	states, err := reviewstate.List(ctx, service)
	if err != nil {
		return false, clierror.Wrap(clierror.Failure, err, "list tracked reviews before sync")
	}
	ctx = withInsecureHTTPWarningOnce(ctx)
	type mergedReview struct {
		state  reviewstate.State
		remote review.Review
	}
	merged := make([]mergedReview, 0, 1)
	for _, state := range states {
		switch state.Stage {
		case reviewstate.StagePicked, reviewstate.StageClosed, reviewstate.StageCleaned:
			continue
		}
		refreshed, remote, refreshErr := a.refreshReview(ctx, service, state.Branch)
		if refreshErr != nil {
			return false, clierror.Wrap(clierror.Code(refreshErr), refreshErr, "refresh review %s before sync; use --base-only only to intentionally skip review cleanup", state.Branch)
		}
		if remote.Status == review.StatusMerged {
			merged = append(merged, mergedReview{state: refreshed, remote: remote})
		}
	}
	if len(merged) == 0 {
		return false, nil
	}
	if len(merged) > 1 {
		branches := make([]string, 0, len(merged))
		for _, item := range merged {
			branches = append(branches, item.state.Branch)
		}
		return false, clierror.New(clierror.Conflict, "multiple merged reviews require an explicit choice: %s; run 'kit review finish <branch>'", strings.Join(branches, ", "))
	}
	item := merged[0]
	err = a.reviewFinishResolvedWithService(ctx, global, reviewFinishOptions{
		branch: item.state.Branch, displayName: "sync",
	}, service, item.state, item.remote)
	return true, err
}

func (a *Application) previewMergedReviewBeforeSync(ctx context.Context, global globalOptions, service gitservice.Service) (bool, error) {
	states, err := reviewstate.List(ctx, service)
	if err != nil {
		return false, clierror.Wrap(clierror.Failure, err, "list tracked reviews before sync preview")
	}
	ctx = withInsecureHTTPWarningOnce(ctx)
	type mergedReview struct {
		state  reviewstate.State
		remote review.Review
	}
	merged := make([]mergedReview, 0, 1)
	for _, state := range states {
		switch state.Stage {
		case reviewstate.StagePicked, reviewstate.StageClosed, reviewstate.StageCleaned:
			continue
		}
		inspected, remote, inspectErr := a.inspectReview(ctx, service, state.Branch)
		if inspectErr != nil {
			return false, clierror.Wrap(clierror.Code(inspectErr), inspectErr, "inspect review %s before sync preview; use --base-only only to intentionally skip review cleanup", state.Branch)
		}
		if remote.Status == review.StatusMerged {
			merged = append(merged, mergedReview{state: inspected, remote: remote})
		}
	}
	if len(merged) == 0 {
		return false, nil
	}
	if len(merged) > 1 {
		branches := make([]string, 0, len(merged))
		for _, item := range merged {
			branches = append(branches, item.state.Branch)
		}
		return false, clierror.New(clierror.Conflict, "multiple merged reviews require an explicit choice: %s; run 'kit review finish <branch>'", strings.Join(branches, ", "))
	}
	item := merged[0]
	if item.remote.SourceSHA == "" || item.remote.SourceSHA != item.state.PublishedTip {
		return false, clierror.New(clierror.Conflict, "provider review head differs from the submitted commit; refusing sync preview")
	}
	if item.remote.MergeSHA == "" {
		return false, clierror.New(clierror.Conflict, "provider did not report a merge commit; refusing sync preview")
	}
	config := service.WorkflowConfig(ctx)
	var plan *workflow.SyncResult
	if item.state.Stage != reviewstate.StageSynced {
		trusted := make(map[string]struct{}, len(item.state.SourceCommits))
		for _, hash := range item.state.SourceCommits {
			trusted[strings.ToLower(hash)] = struct{}{}
		}
		planned, planErr := workflow.Sync(ctx, service, workflow.SyncOptions{
			Config: config, DryRun: true, TrustedAppliedHashes: trusted,
		})
		if planErr != nil {
			return false, clierror.Wrap(clierror.Conflict, planErr, "plan merged review sync")
		}
		plan = &planned
	} else if err := service.Fetch(ctx, config.Remote); err != nil {
		return false, clierror.Wrap(clierror.Failure, err, "fetch before sync preview")
	}
	remoteBase := config.Remote + "/" + config.Base
	mergeObserved, err := service.IsAncestor(ctx, item.remote.MergeSHA, remoteBase)
	if err != nil {
		return false, clierror.Wrap(clierror.Failure, err, "verify merged review during sync preview")
	}
	if !mergeObserved {
		return false, clierror.New(clierror.Conflict, "provider reports the review merged, but %s does not contain merge commit %s yet", remoteBase, item.remote.MergeSHA)
	}
	if exists, existsErr := service.LocalBranchExists(ctx, item.state.Branch); existsErr != nil {
		return false, clierror.Wrap(clierror.Failure, existsErr, "check review branch during sync preview")
	} else if exists {
		localTip, tipErr := service.RevisionHash(ctx, item.state.Branch)
		if tipErr != nil {
			return false, clierror.Wrap(clierror.Failure, tipErr, "read review branch during sync preview")
		}
		if localTip != item.state.PublishedTip {
			return false, clierror.New(clierror.Conflict, "local review branch changed after submit; refusing sync preview")
		}
	}
	result := reviewResult{
		State: item.state, Sync: plan,
		Warnings: []string{"dry-run: work 동기화 후 local review branch를 정리할 예정입니다."},
	}
	if global.json {
		return true, writeJSON(a.IO.Out, result)
	}
	if plan != nil {
		printSyncResult(a.renderer(global), *plan)
	} else {
		a.renderer(global).Command("sync")
	}
	renderer := a.renderer(global)
	renderer.Success("Review", fmt.Sprintf("%s → %s · 머지 확인", item.state.Branch, item.state.TargetBranch))
	renderer.Pending("Cleanup", item.state.Branch+" · local branch 정리 예정")
	return true, nil
}

func syncCommandError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		if err != nil {
			return clierror.Wrap(clierror.Interrupt, err, "sync interrupted after restoring repository state")
		}
		return clierror.New(clierror.Interrupt, "sync interrupted after repository state was finalized; run 'kit status' to verify")
	}
	if err != nil {
		return clierror.Wrap(clierror.Conflict, err, "sync failed")
	}
	return nil
}

func printSyncResult(renderer ui.Renderer, result workflow.SyncResult) {
	action := "변경 없음"
	if result.BaseUpdated {
		action = "원격 기준으로 갱신"
	}
	renderer.Command("sync")
	renderer.Success("Base", fmt.Sprintf("%s · %s", result.Base, action))
	renderer.Success("Work", fmt.Sprintf("머지된 작업 %d 제거 · 작업 %d 유지", result.AppliedDropped, result.PendingKept))
	if result.Skipped > 0 {
		renderer.Warning("Skipped", fmt.Sprintf("%d개 커밋", result.Skipped))
	}
	if result.DryRun {
		renderer.Field("Mode", "dry-run")
	}
	if result.BackupBranch != "" {
		renderer.Field("Backup", result.BackupBranch)
	}
}

type publishResult struct {
	Provider  string `json:"provider"`
	Remote    string `json:"remote"`
	Branch    string `json:"branch"`
	Target    string `json:"target"`
	Pushed    bool   `json:"pushed"`
	ReviewURL string `json:"review_url,omitempty"`
}

func (a *Application) gitPublish(ctx context.Context, global globalOptions, args []string) error {
	dryRun := false
	for len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Fprintln(a.IO.Out, "Usage: kit git publish [--dry-run] [--yes] [--json] [--cwd <path>]")
			return nil
		}
		if consumed, err := parseGlobal(&global, args); err != nil {
			return err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		if args[0] == "--dry-run" {
			dryRun, args = true, args[1:]
			continue
		}
		return clierror.New(clierror.Usage, "unknown git publish option %q", args[0])
	}
	if global.json && !global.yes && !dryRun {
		return clierror.New(clierror.Usage, "git publish --json requires --yes or --dry-run")
	}
	service, err := a.validatedGit(ctx, global.cwd)
	if err != nil {
		return err
	}
	config := service.WorkflowConfig(ctx)
	_, branch, err := service.Head(ctx)
	if err != nil || branch == "" {
		return clierror.New(clierror.Failure, "git publish requires a local branch checkout")
	}
	if branch == config.Stable || branch == config.Base || branch == config.Source {
		return clierror.New(clierror.Failure, "refusing to publish protected workflow branch %q", branch)
	}
	clean, err := service.IsClean(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check working tree")
	}
	if !clean {
		return clierror.New(clierror.Failure, "working tree has changes; commit or stash them before publish")
	}
	remoteURL, err := service.RemoteURL(ctx, config.Remote)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "read remote %s", config.Remote)
	}
	repository := hosting.Resolve(config.Provider, remoteURL)
	upstream, err := service.Upstream(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "read branch upstream")
	}
	remoteExists, err := service.RemoteBranchExists(ctx, config.Remote, branch)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check remote branch")
	}
	expectedUpstream := config.Remote + "/" + branch
	if upstream != "" && upstream != expectedUpstream {
		return clierror.New(clierror.Conflict, "current branch tracks %s, expected %s", upstream, expectedUpstream)
	}
	if remoteExists && upstream != expectedUpstream {
		return clierror.New(clierror.Conflict, "remote branch %s already exists but is not this branch's upstream", expectedUpstream)
	}
	result := publishResult{Provider: repository.Provider, Remote: config.Remote, Branch: branch, Target: config.Base, ReviewURL: repository.ReviewURL(branch, config.Base)}
	if dryRun {
		if global.json {
			return writeJSON(a.IO.Out, result)
		}
		printPublishResult(a.renderer(global), result)
		return nil
	}
	if !global.yes {
		printPublishResult(a.renderer(global), result)
		ok, err := confirm(a.IO.In, a.IO.Out, "현재 브랜치를 push하시겠습니까? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}
	if err := service.PushCurrent(ctx, config.Remote, branch, upstream == ""); err != nil {
		return clierror.Wrap(clierror.Failure, err, "push %s", branch)
	}
	result.Pushed = true
	if global.json {
		return writeJSON(a.IO.Out, result)
	}
	printPublishResult(a.renderer(global), result)
	return nil
}

func printPublishResult(renderer ui.Renderer, result publishResult) {
	renderer.Command("git publish")
	if result.Pushed {
		renderer.Success("Push", result.Remote+"/"+result.Branch)
	} else {
		renderer.Field("Push", result.Remote+"/"+result.Branch+" · 예정")
	}
	renderer.Field("Provider", result.Provider)
	renderer.Field("Target", result.Target)
	if result.ReviewURL != "" {
		renderer.Field("Review URL", result.ReviewURL)
	}
	if result.Pushed && (result.Provider == "gitea" || result.Provider == "gitlab" || result.Provider == "forgejo") {
		renderer.Next("kit review submit")
	}
}

func confirm(in io.Reader, out io.Writer, prompt string) (bool, error) {
	return confirmReaderContext(context.Background(), bufio.NewReader(in), out, prompt)
}

func confirmReaderContext(ctx context.Context, reader *bufio.Reader, out io.Writer, prompt string) (bool, error) {
	fmt.Fprint(out, prompt)
	answer, err := readStringContext(ctx, reader)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, clierror.New(clierror.Interrupt, "interrupted while waiting for confirmation")
	}
	if err != nil && err != io.EOF {
		return false, clierror.Wrap(clierror.Failure, err, "read confirmation")
	}
	answer = strings.TrimSpace(answer)
	return answer == "y" || answer == "Y", nil
}

func (a *Application) resolveSyncConflict(ctx context.Context, reader *bufio.Reader, conflict workflow.SyncConflict) (workflow.SyncConflictAction, error) {
	for {
		fmt.Fprintf(a.IO.Out, "\n%s (%s)를 재적용할 수 없습니다.\n", conflict.Commit.ShortHash, ui.SafeText(conflict.Commit.Subject))
		if len(conflict.Unresolved) > 0 {
			fmt.Fprintf(a.IO.Out, "해결되지 않은 파일: %s\n", strings.Join(conflict.Unresolved, ", "))
		}
		if conflict.ContinueErr != nil {
			fmt.Fprintf(a.IO.Out, "%v\n", conflict.ContinueErr)
		}
		fmt.Fprint(a.IO.Out, "충돌을 해결하고 git add한 뒤 [c]ontinue, [s]kip, [a]bort: ")
		action, err := readStringContext(ctx, reader)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return workflow.SyncConflictAbort, fmt.Errorf("sync interrupted; safely aborting and restoring original work")
			}
			if err == io.EOF {
				return workflow.SyncConflictAbort, fmt.Errorf("input ended; sync was safely aborted and original work was kept")
			}
			return workflow.SyncConflictAbort, fmt.Errorf("read conflict action: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(action)) {
		case "c", "continue":
			return workflow.SyncConflictContinue, nil
		case "s", "skip":
			return workflow.SyncConflictSkip, nil
		case "a", "abort":
			return workflow.SyncConflictAbort, nil
		default:
			fmt.Fprintln(a.IO.Out, "c, s, a 중 하나를 선택하세요.")
		}
	}
}

func readStringContext(ctx context.Context, reader *bufio.Reader) (string, error) {
	type readResult struct {
		text string
		err  error
	}
	result := make(chan readResult, 1)
	go func() {
		text, err := reader.ReadString('\n')
		result <- readResult{text: text, err: err}
	}()
	select {
	case read := <-result:
		return read.text, read.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (a *Application) gitWork(ctx context.Context, global globalOptions, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(a.IO.Out, `Usage: kit git work <command>

Commands:
  refresh   Rebuild work on the current base with pending commits only
  backups   List backups created by sync or refresh
  backup    Create a backup of the configured work branch
  restore   Restore work from a kit backup branch
  cleanup   Remove work backup branches (--dry-run, --all)
`)
		return nil
	}
	command, rest := args[0], args[1:]
	filtered := rest[:0]
	for len(rest) > 0 {
		if consumed, err := parseGlobal(&global, rest); err != nil {
			return err
		} else if consumed > 0 {
			rest = rest[consumed:]
			continue
		}
		filtered = append(filtered, rest[0])
		rest = rest[1:]
	}
	rest = filtered
	service, err := a.validatedGit(ctx, global.cwd)
	if err != nil {
		return err
	}
	config := service.WorkflowConfig(ctx)
	switch command {
	case "refresh":
		dryRun := false
		for len(rest) > 0 {
			if rest[0] == "--dry-run" {
				dryRun, rest = true, rest[1:]
				continue
			}
			return clierror.New(clierror.Usage, "unknown git work refresh option %q", rest[0])
		}
		if global.json && !global.yes && !dryRun {
			return clierror.New(clierror.Usage, "git work refresh --json requires --yes or --dry-run")
		}
		plan, err := workflow.Refresh(ctx, service, config, true)
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "plan work refresh")
		}
		if dryRun {
			if global.json {
				return writeJSON(a.IO.Out, plan)
			}
			printSyncResult(a.renderer(global), plan)
			return nil
		}
		if !global.yes {
			printSyncResult(a.renderer(global), plan)
			ok, err := confirm(a.IO.In, a.IO.Out, "work를 재구성하시겠습니까? [y/N] ")
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(a.IO.Out, "취소되었습니다.")
				return nil
			}
		}
		result, err := workflow.Refresh(ctx, service, config, false)
		if err != nil {
			return clierror.Wrap(clierror.Conflict, err, "work refresh failed")
		}
		if global.json {
			return writeJSON(a.IO.Out, result)
		}
		printSyncResult(a.renderer(global), result)
		return nil
	case "backups":
		if len(rest) != 0 {
			return clierror.New(clierror.Usage, "git work backups accepts no arguments")
		}
		backups, err := listWorkBackupRefs(ctx, service, config.Source)
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "list work backups")
		}
		if global.json {
			return writeJSON(a.IO.Out, backups)
		}
		renderer := a.renderer(global)
		renderer.Command("backup list")
		renderer.Section("Work · " + config.Source)
		if len(backups) == 0 {
			renderer.Field("Backup", "저장된 backup 없음")
			return nil
		}
		for _, backup := range backups {
			renderer.Pending("Backup", backup)
		}
		return nil
	case "backup":
		if len(rest) != 0 {
			return clierror.New(clierror.Usage, "git work backup accepts no arguments")
		}
		return a.createWorkBackup(ctx, global, service, config)
	case "restore":
		if len(rest) != 1 {
			return clierror.New(clierror.Usage, "git work restore requires a backup branch")
		}
		return a.restoreWorkBackup(ctx, global, service, config, rest[0])
	case "cleanup":
		all := false
		dryRun := false
		for len(rest) > 0 {
			switch rest[0] {
			case "--all":
				all, rest = true, rest[1:]
			case "--dry-run":
				dryRun, rest = true, rest[1:]
			default:
				return clierror.New(clierror.Usage, "unknown git work cleanup option %q", rest[0])
			}
		}
		if global.json && !global.yes && !dryRun {
			return clierror.New(clierror.Usage, "git work cleanup --json requires --yes or --dry-run")
		}
		return a.cleanupWorkBackups(ctx, global, service, config, all, dryRun)
	default:
		return clierror.New(clierror.Usage, "unknown git work command %q", command)
	}
}

func (a *Application) createWorkBackup(ctx context.Context, global globalOptions, service gitservice.Service, config gitservice.WorkflowConfig) error {
	hash, err := service.RevisionHash(ctx, config.Source)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "read %s", config.Source)
	}
	short := hash
	if len(short) > 8 {
		short = short[:8]
	}
	name, err := gitservice.FormatWorkBackupRef(config.Source, gitservice.WorkBackupManual, short)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "format work backup")
	}
	if exists, _ := service.LocalBranchExists(ctx, name); exists {
		return clierror.New(clierror.Conflict, "backup %s already exists", name)
	}
	if err := service.CreateBranchAt(ctx, name, hash); err != nil {
		return clierror.Wrap(clierror.Failure, err, "create work backup")
	}
	renderer := a.renderer(global)
	renderer.Command("backup create")
	renderer.Success("Backup", name)
	return nil
}

type workCleanupResult struct {
	Source   string            `json:"source"`
	All      bool              `json:"all"`
	DryRun   bool              `json:"dry_run"`
	Branches []string          `json:"branches"`
	Deleted  []string          `json:"deleted"`
	Failed   map[string]string `json:"failed,omitempty"`
}

func (a *Application) cleanupWorkBackups(ctx context.Context, global globalOptions, service gitservice.Service, config gitservice.WorkflowConfig, all, dryRun bool) error {
	backups, err := listWorkBackupRefs(ctx, service, config.Source)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "list work backups")
	}
	selected := backups
	if !all {
		selected = make([]string, 0, len(backups))
		for _, backup := range backups {
			parsed, ok := gitservice.ParseWorkBackupRef(backup, config.Source)
			if ok && parsed.Kind == gitservice.WorkBackupAuto {
				selected = append(selected, backup)
			}
		}
	}
	result := workCleanupResult{
		Source:   config.Source,
		All:      all,
		DryRun:   dryRun,
		Branches: selected,
		Deleted:  []string{},
	}
	if dryRun || len(selected) == 0 {
		if global.json {
			return writeJSON(a.IO.Out, result)
		}
		if len(selected) == 0 {
			renderer := a.renderer(global)
			renderer.Command("backup cleanup")
			renderer.Field("Backup", "정리할 backup 없음")
			return nil
		}
		renderer := a.renderer(global)
		renderer.Command("backup cleanup")
		renderer.Section("삭제 예정")
		for _, backup := range selected {
			renderer.Pending("Backup", backup)
		}
		return nil
	}
	if !global.yes {
		ok, err := confirm(a.IO.In, a.IO.Out, fmt.Sprintf("%d개의 work backup을 삭제하시겠습니까? [y/N] ", len(selected)))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}
	for _, backup := range selected {
		if err := service.DeleteBranch(ctx, backup, true); err != nil {
			if result.Failed == nil {
				result.Failed = make(map[string]string)
			}
			result.Failed[backup] = err.Error()
			continue
		}
		result.Deleted = append(result.Deleted, backup)
	}
	if len(result.Failed) > 0 {
		remaining := make([]string, 0, len(result.Failed))
		for _, backup := range selected {
			if _, failed := result.Failed[backup]; failed {
				remaining = append(remaining, backup)
			}
		}
		if global.json {
			if err := writeJSON(a.IO.Out, result); err != nil {
				return err
			}
		}
		return clierror.New(clierror.Failure, "work backup cleanup was incomplete; remaining backups: %s", strings.Join(remaining, ", "))
	}
	if global.json {
		return writeJSON(a.IO.Out, result)
	}
	renderer := a.renderer(global)
	renderer.Command("backup cleanup")
	for _, backup := range result.Deleted {
		renderer.Success("Deleted", backup)
	}
	return nil
}

func listWorkBackupRefs(ctx context.Context, service gitservice.Service, source string) ([]string, error) {
	refs, err := service.ListRefs(ctx, "kit/backup")
	if err != nil {
		return nil, err
	}
	backups := make([]string, 0, len(refs))
	for _, ref := range refs {
		if _, ok := gitservice.ParseWorkBackupRef(ref, source); ok {
			backups = append(backups, ref)
		}
	}
	return backups, nil
}

func (a *Application) restoreWorkBackup(ctx context.Context, global globalOptions, service gitservice.Service, config gitservice.WorkflowConfig, backup string) error {
	if _, ok := gitservice.ParseWorkBackupRef(backup, config.Source); !ok {
		return clierror.New(clierror.Usage, "backup %q is not a valid backup owned by source %q", backup, config.Source)
	}
	if err := service.VerifyRevision(ctx, backup); err != nil {
		return clierror.Wrap(clierror.Failure, err, "backup %q was not found", backup)
	}
	clean, err := service.IsClean(ctx)
	if err != nil || !clean {
		return clierror.New(clierror.Failure, "working tree must be clean before restore")
	}
	if !global.yes {
		ok, err := confirm(a.IO.In, a.IO.Out, fmt.Sprintf("%s를 %s로 복원하시겠습니까? [y/N] ", config.Source, backup))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}
	originalHash, originalBranch, err := service.Head(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "record current checkout")
	}
	backupHash, err := service.RevisionHash(ctx, backup)
	if err != nil {
		return err
	}
	currentSourceHash, err := service.RevisionHash(ctx, config.Source)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "read current %s", config.Source)
	}
	short := currentSourceHash
	if len(short) > 8 {
		short = short[:8]
	}
	safetyBackup, err := gitservice.FormatWorkBackupRef(config.Source, gitservice.WorkBackupBeforeRestore, time.Now().UTC().Format("20060102-150405")+"-"+short)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "format pre-restore safety backup")
	}
	if err := service.CreateBranchAt(ctx, safetyBackup, currentSourceHash); err != nil {
		return clierror.Wrap(clierror.Failure, err, "create pre-restore safety backup")
	}
	fail := func(cause error) error {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rollbackVerified, backupRemoved, rollbackErr := rollbackWorkRestore(rollbackCtx, service, config.Source, currentSourceHash, originalHash, originalBranch, safetyBackup)
		if !rollbackVerified {
			return clierror.Wrap(clierror.Failure, cause, "restore %s failed; rollback could not be verified and safety backup remains at %s: %v", config.Source, safetyBackup, rollbackErr)
		}
		if rollbackErr != nil {
			return clierror.Wrap(clierror.Failure, cause, "restore %s failed; original source and checkout were restored, but safety backup remains at %s: %v", config.Source, safetyBackup, rollbackErr)
		}
		if backupRemoved {
			return clierror.Wrap(clierror.Failure, cause, "restore %s failed; original source and checkout were restored; temporary safety backup %s was removed", config.Source, safetyBackup)
		}
		return clierror.Wrap(clierror.Failure, cause, "restore %s failed; original source and checkout were restored; safety backup remains at %s", config.Source, safetyBackup)
	}
	if originalBranch == config.Source {
		if err := service.SwitchDetach(ctx, originalHash); err != nil {
			return fail(fmt.Errorf("detach before restore: %w", err))
		}
	}
	if err := service.ForceBranch(ctx, config.Source, backupHash); err != nil {
		return fail(fmt.Errorf("move %s to %s: %w", config.Source, backup, err))
	}
	if originalBranch == config.Source {
		if err := service.Switch(ctx, config.Source); err != nil {
			return fail(fmt.Errorf("switch to restored %s: %w", config.Source, err))
		}
	}
	if restoredHash, err := service.RevisionHash(ctx, config.Source); err != nil {
		return fail(fmt.Errorf("verify restored %s: %w", config.Source, err))
	} else if restoredHash != backupHash {
		return fail(fmt.Errorf("verify restored %s: got %s, want %s", config.Source, restoredHash, backupHash))
	}
	if headHash, headBranch, err := service.Head(ctx); err != nil {
		return fail(fmt.Errorf("verify checkout after restore: %w", err))
	} else {
		expectedHash := originalHash
		if originalBranch == config.Source {
			expectedHash = backupHash
		}
		if headHash != expectedHash || headBranch != originalBranch {
			return fail(fmt.Errorf("verify checkout after restore: got branch %q at %s, want branch %q at %s", headBranch, headHash, originalBranch, expectedHash))
		}
	}
	renderer := a.renderer(global)
	renderer.Command("backup restore")
	renderer.Success("Work", fmt.Sprintf("%s → %s", config.Source, backup))
	renderer.Field("Safety", safetyBackup)
	return nil
}

func rollbackWorkRestore(ctx context.Context, service gitservice.Service, source, sourceHash, originalHash, originalBranch, safetyBackup string) (bool, bool, error) {
	var rollbackErrors []string
	_, currentBranch, err := service.Head(ctx)
	if err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("read checkout: %v", err))
	} else if currentBranch == source {
		if err := service.SwitchDetach(ctx, sourceHash); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Sprintf("detach %s: %v", source, err))
		}
	}
	if err := service.ForceBranch(ctx, source, sourceHash); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("restore source ref %s: %v", source, err))
	}
	if originalBranch != "" {
		if err := service.Switch(ctx, originalBranch); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Sprintf("restore original checkout %s: %v", originalBranch, err))
		}
	} else if err := service.SwitchDetach(ctx, originalHash); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("restore detached checkout %s: %v", originalHash, err))
	}
	if restoredSource, err := service.RevisionHash(ctx, source); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("verify source ref %s: %v", source, err))
	} else if restoredSource != sourceHash {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("verify source ref %s: got %s, want %s", source, restoredSource, sourceHash))
	}
	if restoredHash, restoredBranch, err := service.Head(ctx); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("verify original checkout: %v", err))
	} else if restoredHash != originalHash || restoredBranch != originalBranch {
		rollbackErrors = append(rollbackErrors, fmt.Sprintf("verify original checkout: got branch %q at %s, want branch %q at %s", restoredBranch, restoredHash, originalBranch, originalHash))
	}
	if len(rollbackErrors) > 0 {
		return false, false, fmt.Errorf("%s", strings.Join(rollbackErrors, "; "))
	}
	if err := service.DeleteBranch(ctx, safetyBackup, true); err != nil {
		return true, false, fmt.Errorf("remove safety backup %s: %w", safetyBackup, err)
	}
	if exists, err := service.LocalBranchExists(ctx, safetyBackup); err != nil {
		return true, false, fmt.Errorf("verify safety backup removal %s: %w", safetyBackup, err)
	} else if exists {
		return true, false, fmt.Errorf("verify safety backup removal %s: branch still exists", safetyBackup)
	}
	return true, true, nil
}

package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
	"kit/internal/review"
	"kit/internal/reviewstate"
	"kit/internal/ui"
	"kit/internal/workflow"
)

type reviewSubmitOptions struct {
	title              string
	description        string
	descriptionFile    string
	draft              bool
	wait               bool
	removeSourceBranch bool
	confirmed          bool
	pushAttempted      *bool
}

type reviewWaitOptions struct {
	branch   string
	interval time.Duration
	timeout  time.Duration
}

type reviewFinishOptions struct {
	branch      string
	forceDelete bool
	confirmed   bool
	displayName string
}

type reviewResult struct {
	State    reviewstate.State    `json:"state"`
	Pushed   bool                 `json:"pushed,omitempty"`
	Reused   bool                 `json:"reused,omitempty"`
	Sync     *workflow.SyncResult `json:"sync,omitempty"`
	Warnings []string             `json:"warnings,omitempty"`
}

type insecureHTTPWarningContextKey struct{}

const insecureHTTPWarning = "Gitea token과 review API 데이터가 암호화되지 않은 HTTP로 전송됩니다."

func (a *Application) gitReview(ctx context.Context, global globalOptions, args []string) error {
	var err error
	global, args, err = parseLeadingGlobals(global, args)
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printReviewHelp(a.IO.Out)
		return nil
	}
	command, rest := args[0], args[1:]
	if len(rest) == 1 && (rest[0] == "-h" || rest[0] == "--help") {
		printReviewHelp(a.IO.Out)
		return nil
	}
	ctx = withInsecureHTTPWarningOnce(ctx)
	switch command {
	case "submit":
		opts, parsedGlobal, err := parseReviewSubmit(global, rest)
		if err != nil {
			return err
		}
		return a.reviewSubmit(ctx, parsedGlobal, opts, nil)
	case "status":
		branch, parsedGlobal, err := parseReviewBranchCommand(global, rest, "status")
		if err != nil {
			return err
		}
		return a.reviewStatus(ctx, parsedGlobal, branch)
	case "wait":
		opts, parsedGlobal, err := parseReviewWait(global, rest)
		if err != nil {
			return err
		}
		return a.reviewWait(ctx, parsedGlobal, opts)
	case "finish":
		opts, parsedGlobal, err := parseReviewFinish(global, rest)
		if err != nil {
			return err
		}
		return a.reviewFinish(ctx, parsedGlobal, opts)
	case "list":
		parsedGlobal, rest, err := parseAllGlobals(global, rest)
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return clierror.New(clierror.Usage, "review list accepts no arguments")
		}
		return a.reviewList(ctx, parsedGlobal)
	default:
		return clierror.New(clierror.Usage, "unknown review command %q", command)
	}
}

func parseReviewSubmit(global globalOptions, args []string) (reviewSubmitOptions, globalOptions, error) {
	opts := reviewSubmitOptions{removeSourceBranch: true}
	for len(args) > 0 {
		if consumed, err := parseGlobal(&global, args); err != nil {
			return opts, global, err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		switch args[0] {
		case "--title", "--description", "--description-file":
			if len(args) < 2 || args[1] == "" {
				return opts, global, clierror.New(clierror.Usage, "%s requires a value", args[0])
			}
			switch args[0] {
			case "--title":
				opts.title = args[1]
			case "--description":
				opts.description = args[1]
			case "--description-file":
				opts.descriptionFile = args[1]
			}
			args = args[2:]
		case "--draft":
			opts.draft, args = true, args[1:]
		case "--wait":
			opts.wait, args = true, args[1:]
		case "--remove-source-branch":
			opts.removeSourceBranch, args = true, args[1:]
		case "--keep-source-branch":
			opts.removeSourceBranch, args = false, args[1:]
		default:
			return opts, global, clierror.New(clierror.Usage, "unknown review submit option %q", args[0])
		}
	}
	if opts.description != "" && opts.descriptionFile != "" {
		return opts, global, clierror.New(clierror.Usage, "--description and --description-file cannot be used together")
	}
	return opts, global, nil
}

func parseReviewBranchCommand(global globalOptions, args []string, command string) (string, globalOptions, error) {
	global, args, err := parseAllGlobals(global, args)
	if err != nil {
		return "", global, err
	}
	if len(args) > 1 {
		return "", global, clierror.New(clierror.Usage, "review %s accepts at most one branch", command)
	}
	if len(args) == 1 {
		return args[0], global, nil
	}
	return "", global, nil
}

func parseReviewWait(global globalOptions, args []string) (reviewWaitOptions, globalOptions, error) {
	opts := reviewWaitOptions{interval: 15 * time.Second}
	positionals := []string{}
	for len(args) > 0 {
		if consumed, err := parseGlobal(&global, args); err != nil {
			return opts, global, err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		if args[0] == "--interval" || args[0] == "--timeout" {
			if len(args) < 2 {
				return opts, global, clierror.New(clierror.Usage, "%s requires a duration", args[0])
			}
			duration, err := time.ParseDuration(args[1])
			if err != nil || duration <= 0 {
				return opts, global, clierror.New(clierror.Usage, "%s requires a positive duration", args[0])
			}
			if args[0] == "--interval" {
				if duration < 5*time.Second {
					return opts, global, clierror.New(clierror.Usage, "--interval must be at least 5s")
				}
				opts.interval = duration
			} else {
				opts.timeout = duration
			}
			args = args[2:]
			continue
		}
		if strings.HasPrefix(args[0], "-") {
			return opts, global, clierror.New(clierror.Usage, "unknown review wait option %q", args[0])
		}
		positionals, args = append(positionals, args[0]), args[1:]
	}
	if len(positionals) > 1 {
		return opts, global, clierror.New(clierror.Usage, "review wait accepts at most one branch")
	}
	if len(positionals) == 1 {
		opts.branch = positionals[0]
	}
	return opts, global, nil
}

func parseReviewFinish(global globalOptions, args []string) (reviewFinishOptions, globalOptions, error) {
	opts := reviewFinishOptions{}
	positionals := []string{}
	for len(args) > 0 {
		if consumed, err := parseGlobal(&global, args); err != nil {
			return opts, global, err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		if args[0] == "--force-delete" {
			opts.forceDelete, args = true, args[1:]
			continue
		}
		if strings.HasPrefix(args[0], "-") {
			return opts, global, clierror.New(clierror.Usage, "unknown review finish option %q", args[0])
		}
		positionals, args = append(positionals, args[0]), args[1:]
	}
	if len(positionals) > 1 {
		return opts, global, clierror.New(clierror.Usage, "review finish accepts at most one branch")
	}
	if len(positionals) == 1 {
		opts.branch = positionals[0]
	}
	return opts, global, nil
}

func parseAllGlobals(global globalOptions, args []string) (globalOptions, []string, error) {
	result := make([]string, 0, len(args))
	for len(args) > 0 {
		if consumed, err := parseGlobal(&global, args); err != nil {
			return global, nil, err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		result, args = append(result, args[0]), args[1:]
	}
	return global, result, nil
}

func (a *Application) reviewSubmit(ctx context.Context, global globalOptions, opts reviewSubmitOptions, serviceOverride *gitservice.Service) error {
	ctx = withInsecureHTTPWarningOnce(ctx)
	if global.json && !global.yes && !opts.confirmed {
		return clierror.New(clierror.Usage, "review submit --json requires --yes")
	}
	service, err := a.reviewGitService(ctx, global, serviceOverride)
	if err != nil {
		return err
	}
	config := service.WorkflowConfig(ctx)
	head, branch, err := service.Head(ctx)
	if err != nil || branch == "" {
		return clierror.New(clierror.Failure, "review submit requires a local branch checkout")
	}
	if isProtectedReviewBranch(branch, config) {
		return clierror.New(clierror.Failure, "refusing to submit protected workflow branch %q", branch)
	}
	clean, err := service.IsClean(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check working tree")
	}
	if !clean {
		return clierror.New(clierror.Failure, "working tree has changes; commit or stash them before review submit")
	}

	state, err := reviewstate.Load(ctx, service, branch)
	if errors.Is(err, reviewstate.ErrNotFound) {
		state = reviewstate.State{Stage: reviewstate.StagePicked, Branch: branch, SourceBranch: config.Source, TargetBranch: config.Base, PickedTip: head}
		if err := reviewstate.Save(ctx, service, state); err != nil {
			return clierror.Wrap(clierror.Failure, err, "save picked review state")
		}
	} else if err != nil {
		return clierror.Wrap(clierror.Failure, err, "load review state")
	} else if state.Stage == reviewstate.StageCleaned && state.PickedTip != head {
		state = reviewstate.State{Stage: reviewstate.StagePicked, Branch: branch, SourceBranch: config.Source, TargetBranch: config.Base, PickedTip: head}
		if err := reviewstate.Save(ctx, service, state); err != nil {
			return clierror.Wrap(clierror.Failure, err, "replace completed review state")
		}
	}
	if state.SourceBranch != config.Source || state.TargetBranch != config.Base {
		return clierror.New(clierror.Conflict, "review workflow changed since pick (source %s, target %s); restore the repository config before submit", state.SourceBranch, state.TargetBranch)
	}

	repository, err := reviewRepository(ctx, service, config)
	if err != nil {
		return err
	}
	warnInsecureHTTP(ctx, a.IO.ErrOut, repository)
	client, err := a.ReviewClient(repository)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "initialize review provider before push")
	}
	preexisting, err := client.FindOpen(ctx, branch, state.TargetBranch)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check existing review before push")
	}
	if len(preexisting) > 1 {
		return clierror.New(clierror.Conflict, "multiple open reviews exist for %s → %s; refusing to choose one", branch, state.TargetBranch)
	}
	title, description := "", ""
	textPrepared := false
	if len(preexisting) == 0 {
		title, description, err = a.reviewText(ctx, service, state, opts)
		if err != nil {
			return err
		}
		textPrepared = true
	} else {
		title = preexisting[0].Title
		if title == "" {
			title = "기존 review 재사용"
		}
	}
	if !opts.confirmed && !global.yes {
		renderer := a.renderer(global)
		renderer.Command("review submit")
		renderer.Field("Source", branch)
		renderer.Field("Target", state.TargetBranch)
		renderer.Field("Title", title)
		ok, err := confirm(a.IO.In, a.IO.Out, "\n브랜치를 push하고 리뷰를 생성하시겠습니까? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}

	upstream, err := service.Upstream(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "read branch upstream")
	}
	expectedUpstream := config.Remote + "/" + branch
	if upstream != "" && upstream != expectedUpstream {
		return clierror.New(clierror.Conflict, "current branch tracks %s, expected %s", upstream, expectedUpstream)
	}
	if opts.pushAttempted != nil {
		*opts.pushAttempted = true
	}
	if err := service.PushCurrent(ctx, config.Remote, branch, upstream == ""); err != nil {
		return clierror.Wrap(clierror.Failure, err, "push %s", branch)
	}
	state.Provider = repository.Provider
	state.Remote = config.Remote
	state.PublishedTip = head
	state.Stage = reviewstate.StagePublished
	if err := reviewstate.Save(ctx, service, state); err != nil {
		return clierror.Wrap(clierror.Failure, err, "save published review state")
	}

	found, err := client.FindOpen(ctx, branch, state.TargetBranch)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "find existing review")
	}
	if len(found) > 1 {
		return clierror.New(clierror.Conflict, "multiple open reviews exist for %s → %s; refusing to choose one", branch, state.TargetBranch)
	}
	reused := len(found) == 1
	remoteReview := review.Review{}
	if reused {
		remoteReview = found[0]
	} else {
		recovered, recoverErr := findPublishedReview(ctx, client, branch, state.TargetBranch, state.PublishedTip)
		if recoverErr != nil {
			return clierror.Wrap(clierror.Failure, recoverErr, "recover review created by an earlier submit")
		}
		if len(recovered) > 1 {
			return clierror.New(clierror.Conflict, "multiple reviews match the published commit for %s; refusing to choose one", branch)
		}
		if len(recovered) == 1 {
			remoteReview = recovered[0]
			reused = true
		}
	}
	if remoteReview.ID == "" {
		if !textPrepared {
			title, description, err = a.reviewText(ctx, service, state, opts)
			if err != nil {
				return err
			}
		}
		remoteReview, err = client.Create(ctx, review.CreateRequest{
			SourceBranch: branch, TargetBranch: state.TargetBranch, Title: title, Description: description,
			Draft: opts.draft, RemoveSourceBranch: opts.removeSourceBranch,
		})
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "create review")
		}
	}
	if err := updateReviewState(&state, remoteReview); err != nil {
		return err
	}
	if err := reviewstate.Save(ctx, service, state); err != nil {
		return clierror.Wrap(clierror.Failure, err, "save open review state")
	}
	result := reviewResult{State: state, Pushed: true, Reused: reused}
	if (repository.Provider == "gitea" || repository.Provider == "forgejo") && opts.removeSourceBranch {
		providerName := "Gitea"
		if repository.Provider == "forgejo" {
			providerName = "Forgejo"
		}
		result.Warnings = append(result.Warnings, providerName+" remote branch cleanup follows the server and repository policy; kit does not delete it directly.")
	}
	if opts.wait {
		if !global.json {
			if err := a.printReviewResult(global, "review submit", result); err != nil {
				return err
			}
		}
		return a.reviewWaitWithService(ctx, global, reviewWaitOptions{branch: branch, interval: 15 * time.Second}, service)
	}
	return a.printReviewResult(global, "review submit", result)
}

func withInsecureHTTPWarningOnce(ctx context.Context) context.Context {
	if _, ok := ctx.Value(insecureHTTPWarningContextKey{}).(*sync.Once); ok {
		return ctx
	}
	return context.WithValue(ctx, insecureHTTPWarningContextKey{}, &sync.Once{})
}

func (a *Application) reviewStatus(ctx context.Context, global globalOptions, branch string) error {
	service, err := a.reviewGitService(ctx, global, nil)
	if err != nil {
		return err
	}
	state, _, err := a.refreshReview(ctx, service, branch)
	if err != nil {
		return err
	}
	return a.printReviewResult(global, "review status", reviewResult{State: state})
}

func (a *Application) reviewWait(ctx context.Context, global globalOptions, opts reviewWaitOptions) error {
	service, err := a.reviewGitService(ctx, global, nil)
	if err != nil {
		return err
	}
	return a.reviewWaitWithService(ctx, global, opts, service)
}

func (a *Application) reviewWaitWithService(ctx context.Context, global globalOptions, opts reviewWaitOptions, service gitservice.Service) error {
	waitCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if opts.interval == 0 {
		opts.interval = 15 * time.Second
	}
	var cancelTimeout context.CancelFunc
	if opts.timeout > 0 {
		waitCtx, cancelTimeout = context.WithTimeout(waitCtx, opts.timeout)
		defer cancelTimeout()
	}
	announced := false
	transientFailures := 0
	for {
		state, _, err := a.refreshReview(waitCtx, service, opts.branch)
		if err != nil {
			if waitCtx.Err() != nil {
				return reviewWaitContextError(waitCtx.Err())
			}
			if !isTransientReviewError(err) {
				return err
			}
			transientFailures++
			if !global.json {
				renderer := a.renderer(global)
				renderer.Warning("Retry", fmt.Sprintf("Gitea API 응답 지연 · %s 후 다시 확인 (%d회)", opts.interval, transientFailures))
			}
		} else {
			transientFailures = 0
			switch state.Status {
			case review.StatusMerged:
				if global.json {
					if global.yes {
						return a.reviewFinishWithService(waitCtx, global, reviewFinishOptions{branch: state.Branch, confirmed: true}, service)
					}
					return writeJSON(a.IO.Out, reviewResult{State: state})
				}
				renderer := a.renderer(global)
				renderer.Notice("머지 완료")
				renderer.Success("Review", fmt.Sprintf("%s → %s", state.Branch, state.TargetBranch))
				if global.yes {
					return a.reviewFinishWithService(waitCtx, global, reviewFinishOptions{branch: state.Branch}, service)
				}
				if isTerminal(a.IO.Out) && a.IO.InFile != nil && isTerminal(a.IO.InFile) {
					finish, err := confirmDefaultYes(a.IO.In, a.IO.Out, "\n동기화와 로컬 브랜치 정리를 진행하시겠습니까? [Y/n] ")
					if err != nil {
						return err
					}
					if finish {
						return a.reviewFinishWithService(waitCtx, global, reviewFinishOptions{branch: state.Branch, confirmed: true}, service)
					}
				}
				renderer.Next("kit review finish " + ui.ShellQuote(state.Branch))
				return nil
			case review.StatusClosed:
				return clierror.New(clierror.Conflict, "review for %s was closed without merge", state.Branch)
			}
			if !announced && !global.json {
				renderer := a.renderer(global)
				renderer.Command("review wait")
				renderer.Field("Review", fmt.Sprintf("%s → %s", state.Branch, state.TargetBranch))
				renderer.Field("Poll", opts.interval.String())
				announced = true
			}
		}
		poll := time.NewTimer(opts.interval)
		select {
		case <-waitCtx.Done():
			poll.Stop()
			return reviewWaitContextError(waitCtx.Err())
		case <-poll.C:
		}
	}
}

func isTransientReviewError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func reviewWaitContextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return clierror.New(clierror.Failure, "review wait timed out; state was kept")
	}
	return clierror.Wrap(clierror.Interrupt, err, "review wait interrupted")
}

func (a *Application) reviewFinish(ctx context.Context, global globalOptions, opts reviewFinishOptions) error {
	service, err := a.reviewGitService(ctx, global, nil)
	if err != nil {
		return err
	}
	return a.reviewFinishWithService(ctx, global, opts, service)
}

func (a *Application) reviewFinishWithService(ctx context.Context, global globalOptions, opts reviewFinishOptions, service gitservice.Service) error {
	if global.json && !global.yes && !opts.confirmed {
		return clierror.New(clierror.Usage, "review finish --json requires --yes")
	}
	state, remoteReview, err := a.refreshReview(ctx, service, opts.branch)
	if err != nil {
		return err
	}
	return a.reviewFinishResolvedWithService(ctx, global, opts, service, state, remoteReview)
}

func (a *Application) reviewFinishResolvedWithService(ctx context.Context, global globalOptions, opts reviewFinishOptions, service gitservice.Service, state reviewstate.State, remoteReview review.Review) error {
	if remoteReview.Status != review.StatusMerged {
		return clierror.New(clierror.Conflict, "review %s is %s; finish requires a merged review", state.Branch, remoteReview.Status)
	}
	if state.Stage == reviewstate.StageCleaned {
		return a.printReviewResult(global, "review finish", reviewResult{State: state})
	}
	config := service.WorkflowConfig(ctx)
	if state.SourceBranch != config.Source || state.TargetBranch != config.Base {
		return clierror.New(clierror.Conflict, "review workflow changed since submit (source %s, target %s); restore the repository config before finish", state.SourceBranch, state.TargetBranch)
	}
	if isProtectedReviewBranch(state.Branch, config) {
		return clierror.New(clierror.Conflict, "refusing to delete protected workflow branch %q", state.Branch)
	}
	clean, err := service.IsClean(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check working tree")
	}
	if !clean {
		return clierror.New(clierror.Conflict, "working tree has changes; commit or stash them before review finish")
	}
	exists, err := service.LocalBranchExists(ctx, state.Branch)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check local review branch")
	}
	if remoteReview.SourceSHA == "" {
		return clierror.New(clierror.Conflict, "provider did not report the merged review head; refusing automatic sync and branch deletion")
	}
	if remoteReview.SourceSHA != state.PublishedTip {
		return clierror.New(clierror.Conflict, "provider review head differs from the submitted commit; submit the branch again")
	}
	if remoteReview.MergeSHA == "" {
		return clierror.New(clierror.Conflict, "provider did not report a merge commit; refusing automatic sync and branch deletion")
	}
	if err := service.Fetch(ctx, config.Remote); err != nil {
		return clierror.Wrap(clierror.Failure, err, "fetch before verifying merged review")
	}
	remoteBase := config.Remote + "/" + config.Base
	if err := verifyRevisions(ctx, service, remoteBase, remoteReview.MergeSHA); err != nil {
		return clierror.Wrap(clierror.Conflict, err, "merged review is not observable on %s yet", remoteBase)
	}
	mergeObserved, err := service.IsAncestor(ctx, remoteReview.MergeSHA, remoteBase)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "verify merged review on %s", remoteBase)
	}
	if !mergeObserved {
		return clierror.New(clierror.Conflict, "provider reports the review merged, but %s does not contain merge commit %s yet; retry after the target branch updates", remoteBase, remoteReview.MergeSHA)
	}
	if exists {
		localTip, err := service.RevisionHash(ctx, state.Branch)
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "read local review branch")
		}
		if localTip != state.PublishedTip {
			return clierror.New(clierror.Conflict, "local review branch changed after submit; run review submit again before finish")
		}
	}
	if !opts.confirmed && !global.yes {
		renderer := a.renderer(global)
		displayName := opts.displayName
		if displayName == "" {
			displayName = "review finish"
		}
		renderer.Command(displayName)
		renderer.Field("Review", fmt.Sprintf("%s → %s · merged", state.Branch, state.TargetBranch))
		renderer.Field("Actions", fmt.Sprintf("%s 동기화 · 로컬 %s 삭제", config.Source, state.Branch))
		ok, err := confirm(a.IO.In, a.IO.Out, "\n동기화와 로컬 브랜치 정리를 진행하시겠습니까? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}

	var syncResult *workflow.SyncResult
	if state.Stage != reviewstate.StageSynced {
		_, currentBranch, err := service.Head(ctx)
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "read current branch")
		}
		if currentBranch != config.Source {
			if err := service.Switch(ctx, config.Source); err != nil {
				return clierror.Wrap(clierror.Failure, err, "switch to %s before sync", config.Source)
			}
		}
		trusted := make(map[string]struct{}, len(state.SourceCommits))
		for _, hash := range state.SourceCommits {
			trusted[strings.ToLower(hash)] = struct{}{}
		}
		var resolver workflow.SyncConflictResolver
		if !global.json {
			reader := bufio.NewReader(a.IO.In)
			resolver = func(ctx context.Context, conflict workflow.SyncConflict) (workflow.SyncConflictAction, error) {
				return a.resolveSyncConflict(ctx, reader, conflict)
			}
		}
		syncCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		result, syncErr := workflow.Sync(syncCtx, service, workflow.SyncOptions{Config: config, Resolver: resolver, TrustedAppliedHashes: trusted})
		stop()
		if syncErr != nil {
			return clierror.Wrap(clierror.Conflict, syncErr, "review was merged, but work sync failed")
		}
		syncResult = &result
		now := time.Now().UTC()
		state.Stage = reviewstate.StageSynced
		state.SyncedAt = &now
		if err := reviewstate.Save(ctx, service, state); err != nil {
			return clierror.Wrap(clierror.Failure, err, "save synced review state")
		}
	}

	if exists {
		if err := service.DeleteBranch(ctx, state.Branch, false); err != nil {
			if !opts.forceDelete {
				return clierror.Wrap(clierror.Conflict, err, "review was synced, but safe branch deletion failed; rerun with --force-delete only after verifying the merged review")
			}
			if err := service.DeleteBranch(ctx, state.Branch, true); err != nil {
				return clierror.Wrap(clierror.Failure, err, "force delete merged review branch")
			}
		}
	}
	now := time.Now().UTC()
	state.Stage = reviewstate.StageCleaned
	state.CleanedAt = &now
	if err := reviewstate.Save(ctx, service, state); err != nil {
		return clierror.Wrap(clierror.Failure, err, "save cleaned review state")
	}
	displayName := opts.displayName
	if displayName == "" {
		displayName = "review finish"
	}
	return a.printReviewResult(global, displayName, reviewResult{State: state, Sync: syncResult})
}

func (a *Application) reviewList(ctx context.Context, global globalOptions) error {
	service, err := a.reviewGitService(ctx, global, nil)
	if err != nil {
		return err
	}
	states, err := reviewstate.List(ctx, service)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "list review states")
	}
	if global.json {
		return writeJSON(a.IO.Out, states)
	}
	renderer := a.renderer(global)
	renderer.Command("review list")
	renderer.Section("추적 중인 리뷰")
	if len(states) == 0 {
		renderer.Field("State", "추적 중인 리뷰가 없습니다.")
		return nil
	}
	for _, state := range states {
		value := fmt.Sprintf("%s → %s · %s", state.Branch, state.TargetBranch, reviewStageLabel(state.Stage))
		switch state.Stage {
		case reviewstate.StageMerged, reviewstate.StageSynced, reviewstate.StageCleaned:
			renderer.Success("Review", value)
		case reviewstate.StageClosed:
			renderer.Warning("Review", value)
		default:
			renderer.Pending("Review", value)
		}
	}
	return nil
}

func (a *Application) refreshReview(ctx context.Context, service gitservice.Service, branch string) (reviewstate.State, review.Review, error) {
	return a.readReview(ctx, service, branch, true)
}

func (a *Application) inspectReview(ctx context.Context, service gitservice.Service, branch string) (reviewstate.State, review.Review, error) {
	return a.readReview(ctx, service, branch, false)
}

func (a *Application) readReview(ctx context.Context, service gitservice.Service, branch string, persist bool) (reviewstate.State, review.Review, error) {
	resolved, err := resolveReviewBranch(ctx, service, branch)
	if err != nil {
		return reviewstate.State{}, review.Review{}, err
	}
	state, err := reviewstate.Load(ctx, service, resolved)
	if err != nil {
		return reviewstate.State{}, review.Review{}, clierror.Wrap(clierror.Failure, err, "load review state")
	}
	if state.Stage == reviewstate.StagePicked {
		return state, review.Review{}, clierror.New(clierror.Conflict, "review branch %s has not been submitted", state.Branch)
	}
	config := service.WorkflowConfig(ctx)
	if state.SourceBranch != config.Source || state.TargetBranch != config.Base {
		return state, review.Review{}, clierror.New(clierror.Conflict, "review workflow changed since submit (source %s, target %s); restore the repository config before refresh", state.SourceBranch, state.TargetBranch)
	}
	repository, err := reviewRepository(ctx, service, config)
	if err != nil {
		return state, review.Review{}, err
	}
	if state.Provider != repository.Provider || state.Remote != config.Remote {
		return state, review.Review{}, clierror.New(clierror.Conflict, "review state provider or remote no longer matches repository config")
	}
	if !reviewStateOriginMatchesRepository(state, repository) {
		return state, review.Review{}, clierror.New(clierror.Conflict, "review state URL origin no longer matches repository remote")
	}
	warnInsecureHTTP(ctx, a.IO.ErrOut, repository)
	client, err := a.ReviewClient(repository)
	if err != nil {
		return state, review.Review{}, clierror.Wrap(clierror.Failure, err, "initialize review provider")
	}
	remoteReview := review.Review{}
	if state.ReviewID == "" {
		found, err := findPublishedReview(ctx, client, state.Branch, state.TargetBranch, state.PublishedTip)
		if err != nil {
			return state, remoteReview, clierror.Wrap(clierror.Failure, err, "recover submitted review")
		}
		if len(found) == 0 {
			return state, remoteReview, clierror.New(clierror.Conflict, "no provider review matched the published commit; rerun review submit")
		}
		if len(found) > 1 {
			return state, remoteReview, clierror.New(clierror.Conflict, "multiple open reviews exist for %s", state.Branch)
		}
		remoteReview = found[0]
	} else {
		remoteReview, err = client.Get(ctx, state.ReviewID)
		if err != nil {
			return state, remoteReview, clierror.Wrap(clierror.Failure, err, "read provider review")
		}
	}
	if err := updateReviewState(&state, remoteReview); err != nil {
		return state, remoteReview, err
	}
	if persist {
		if err := reviewstate.Save(ctx, service, state); err != nil {
			return state, remoteReview, clierror.Wrap(clierror.Failure, err, "save refreshed review state")
		}
	}
	return state, remoteReview, nil
}

func findPublishedReview(ctx context.Context, client review.Client, source, target, publishedTip string) ([]review.Review, error) {
	found, err := client.Find(ctx, source, target)
	if err != nil {
		return nil, err
	}
	matched := make([]review.Review, 0, 1)
	for _, item := range found {
		if item.SourceBranch == source && item.TargetBranch == target && item.SourceSHA != "" && item.SourceSHA == publishedTip {
			matched = append(matched, item)
		}
	}
	return matched, nil
}

func updateReviewState(state *reviewstate.State, remoteReview review.Review) error {
	if remoteReview.ID == "" || remoteReview.Number <= 0 || remoteReview.URL == "" {
		return clierror.New(clierror.Failure, "provider returned incomplete review metadata")
	}
	if remoteReview.SourceBranch != state.Branch || remoteReview.TargetBranch != state.TargetBranch {
		return clierror.New(clierror.Conflict, "provider review source or target does not match local review state")
	}
	state.ReviewID = remoteReview.ID
	state.ReviewNumber = remoteReview.Number
	state.ReviewURL = remoteReview.URL
	state.Status = remoteReview.Status
	state.MergeSHA = remoteReview.MergeSHA
	state.MergedAt = remoteReview.MergedAt
	switch remoteReview.Status {
	case review.StatusOpen:
		if state.Stage != reviewstate.StageSynced && state.Stage != reviewstate.StageCleaned {
			state.Stage = reviewstate.StageOpen
		}
	case review.StatusMerged:
		if state.Stage != reviewstate.StageSynced && state.Stage != reviewstate.StageCleaned {
			state.Stage = reviewstate.StageMerged
		}
	case review.StatusClosed:
		if state.Stage != reviewstate.StageSynced && state.Stage != reviewstate.StageCleaned {
			state.Stage = reviewstate.StageClosed
		}
	default:
		return clierror.New(clierror.Failure, "provider returned unsupported review status %q", remoteReview.Status)
	}
	return nil
}

func (a *Application) reviewGitService(ctx context.Context, global globalOptions, override *gitservice.Service) (gitservice.Service, error) {
	if override != nil {
		return *override, nil
	}
	return a.validatedGit(ctx, global.cwd)
}

func reviewRepository(ctx context.Context, service gitservice.Service, config gitservice.WorkflowConfig) (hosting.Repository, error) {
	remoteURL, err := service.RemoteURL(ctx, config.Remote)
	if err != nil {
		return hosting.Repository{}, clierror.Wrap(clierror.Failure, err, "read remote %s", config.Remote)
	}
	repository := hosting.Resolve(config.Provider, remoteURL)
	repository.AllowInsecureHTTP = config.AllowInsecureHTTP
	if repository.Provider != "gitea" && repository.Provider != "gitlab" && repository.Provider != "forgejo" {
		return hosting.Repository{}, clierror.New(clierror.Failure, "provider %q does not support automated reviews", repository.Provider)
	}
	return repository, nil
}

func warnInsecureHTTP(ctx context.Context, writer io.Writer, repository hosting.Repository) {
	if !repository.InsecureHTTPAllowed() {
		return
	}
	write := func() {
		renderer := ui.Renderer{Writer: writer}
		renderer.Notice("보안 경고")
		renderer.Warning("HTTP", insecureHTTPWarning)
	}
	if once, ok := ctx.Value(insecureHTTPWarningContextKey{}).(*sync.Once); ok {
		once.Do(write)
		return
	}
	write()
}

func reviewStateOriginMatchesRepository(state reviewstate.State, repository hosting.Repository) bool {
	if state.ReviewURL == "" || repository.Host == "" {
		return true
	}
	parsed, err := url.Parse(state.ReviewURL)
	if err != nil {
		return false
	}
	expectedScheme := "https"
	if repository.InsecureHTTPAllowed() {
		expectedScheme = "http"
	}
	return parsed.Scheme == expectedScheme && parsed.Host == repository.Host
}

func resolveReviewBranch(ctx context.Context, service gitservice.Service, branch string) (string, error) {
	if branch != "" {
		return branch, nil
	}
	_, current, err := service.Head(ctx)
	if err == nil && current != "" {
		if _, loadErr := reviewstate.Load(ctx, service, current); loadErr == nil {
			return current, nil
		}
	}
	states, listErr := reviewstate.List(ctx, service)
	if listErr != nil {
		return "", clierror.Wrap(clierror.Failure, listErr, "list review states")
	}
	active := make([]reviewstate.State, 0, len(states))
	for _, state := range states {
		if state.Stage != reviewstate.StageCleaned {
			active = append(active, state)
		}
	}
	if len(active) == 1 {
		return active[0].Branch, nil
	}
	if len(active) == 0 {
		return "", clierror.New(clierror.Failure, "no active review state was found")
	}
	return "", clierror.New(clierror.Usage, "multiple active reviews exist; specify a branch")
}

func (a *Application) reviewText(ctx context.Context, service gitservice.Service, state reviewstate.State, opts reviewSubmitOptions) (string, string, error) {
	commits := make([]gitservice.Commit, 0, len(state.SourceCommits))
	for _, hash := range state.SourceCommits {
		commit, err := service.Commit(ctx, hash)
		if err != nil {
			return "", "", clierror.Wrap(clierror.Failure, err, "read review source commit")
		}
		commits = append(commits, commit)
	}
	if len(commits) == 0 {
		listed, err := service.Candidates(ctx, state.TargetBranch, state.Branch, true)
		if err != nil {
			return "", "", clierror.Wrap(clierror.Failure, err, "list review commits")
		}
		commits = listed
	}
	title := strings.TrimSpace(opts.title)
	if title == "" && len(commits) > 0 {
		title = commits[0].Subject
	}
	if title == "" {
		return "", "", clierror.New(clierror.Usage, "review title is required; use --title")
	}
	description := opts.description
	if opts.descriptionFile != "" {
		path := opts.descriptionFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(service.Dir, path)
		}
		data, err := readReviewDescription(path)
		if err != nil {
			return "", "", clierror.Wrap(clierror.Failure, err, "read review description file")
		}
		description = string(data)
	}
	if description == "" {
		var builder strings.Builder
		builder.WriteString("## 변경 커밋\n\n")
		for _, commit := range commits {
			fmt.Fprintf(&builder, "- `%s` %s\n", commit.ShortHash, commit.Subject)
		}
		description = builder.String()
	}
	return title, description, nil
}

func readReviewDescription(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("description path must be a regular file (symlinks and special files are not allowed)")
	}
	if info.Size() > 1<<20 {
		return nil, errors.New("review description file exceeds 1 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() {
		return nil, errors.New("description path changed before it could be read")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("review description file exceeds 1 MiB")
	}
	return data, nil
}

func (a *Application) printReviewResult(global globalOptions, command string, result reviewResult) error {
	if global.json {
		return writeJSON(a.IO.Out, result)
	}
	renderer := a.renderer(global)
	renderer.Command(command)
	if result.Pushed {
		renderer.Success("Push", result.State.Remote+"/"+result.State.Branch)
	}
	renderer.Section("리뷰")
	if result.State.ReviewNumber > 0 {
		label := "Review"
		marker := "!"
		if result.State.Provider == "gitlab" {
			label = "GitLab MR"
		} else if result.State.Provider == "forgejo" {
			label = "Forgejo PR"
			marker = "#"
		} else if result.State.Provider == "gitea" {
			label = "Gitea PR"
			marker = "#"
		}
		value := fmt.Sprintf("%s%d (%s)", marker, result.State.ReviewNumber, result.State.Status)
		if result.Reused {
			value += " · 기존 리뷰 재사용"
		}
		renderer.Success(label, value)
	}
	renderer.Field("Branch", result.State.Branch)
	renderer.Field("Target", result.State.TargetBranch)
	switch result.State.Stage {
	case reviewstate.StageMerged, reviewstate.StageSynced, reviewstate.StageCleaned:
		renderer.Success("State", reviewStageLabel(result.State.Stage))
	case reviewstate.StageClosed:
		renderer.Warning("State", reviewStageLabel(result.State.Stage))
	default:
		renderer.Pending("State", reviewStageLabel(result.State.Stage))
	}
	if result.State.ReviewURL != "" {
		renderer.Field("URL", result.State.ReviewURL)
	}
	if result.Sync != nil {
		renderer.Success("Sync", fmt.Sprintf("머지된 작업 %d 제거 · 작업 %d 유지", result.Sync.AppliedDropped, result.Sync.PendingKept))
	}
	for _, warning := range result.Warnings {
		renderer.Warning("Notice", warning)
	}
	if result.State.Stage == reviewstate.StageOpen {
		renderer.Next("kit review wait " + ui.ShellQuote(result.State.Branch))
	} else if result.State.Stage == reviewstate.StageMerged {
		renderer.Next("kit review finish " + ui.ShellQuote(result.State.Branch))
	}
	return nil
}

func confirmDefaultYes(in io.Reader, out io.Writer, prompt string) (bool, error) {
	fmt.Fprint(out, prompt)
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, clierror.Wrap(clierror.Failure, err, "read confirmation")
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "" || answer == "y" || answer == "yes", nil
}

func isProtectedReviewBranch(branch string, config gitservice.WorkflowConfig) bool {
	return branch == config.Stable || branch == config.Base || branch == config.Source
}

func printReviewHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: kit review <command>

Commands:
  submit [options]    Push the current branch and create or reuse a Gitea PR
  status [branch]     Refresh one review from its provider
  wait [branch]       Wait until a review is merged or closed
  finish [branch]     Sync work and delete the merged local review branch
  list                List locally tracked reviews

Submit options:
  --title <text>              Override the generated title
  --description <text>        Override the generated description
  --description-file <path>   Read the description from a file (max 1 MiB)
  --draft                     Prefix a Gitea PR title with "WIP: "
  --wait                      Wait for merge after submit
  --keep-source-branch        Suppress cleanup request; Gitea server policy still applies

Wait options:
  --interval <duration>       Poll interval (default 15s, minimum 5s)
  --timeout <duration>        Stop waiting after a duration

Finish options:
  --force-delete              Allow -D only after provider-confirmed merge

Common options:
  --cwd <path>                Git repository directory
  --json                      Print one machine-readable result
  --yes                       Skip mutation confirmation; wait also runs finish
  --no-color                  Disable ANSI colors
`)
}

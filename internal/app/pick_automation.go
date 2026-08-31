package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/pickstate"
	"kit/internal/selector"
)

type automatedPickOptions struct {
	globalOptions
	target     string
	source     string
	base       string
	sourceSet  bool
	baseSet    bool
	fetch      bool
	allowStale bool
	all        bool
	commits    []string
	dryRun     bool
	submit     bool
	localOnly  bool
}

type automatedPickPlan struct {
	Target       string              `json:"target"`
	Source       string              `json:"source"`
	Base         string              `json:"base"`
	Commits      []gitservice.Commit `json:"commits"`
	NeedsSync    bool                `json:"needs_sync"`
	WillPush     bool                `json:"will_push"`
	WillCreatePR bool                `json:"will_create_pr"`
	DryRun       bool                `json:"dry_run"`
}

type automatedLocalPickResult struct {
	Branch  string   `json:"branch"`
	Commits []string `json:"commits"`
}

func pickUsesAutomation(global globalOptions, args []string) bool {
	if global.json {
		return true
	}
	for _, arg := range args {
		if arg == "--commit" || strings.HasPrefix(arg, "--commit=") || arg == "--dry-run" {
			return true
		}
	}
	return false
}

func (a *Application) pickEnhanced(ctx context.Context, global globalOptions, args []string) error {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			printPickHelp(a.IO.Out)
			fmt.Fprint(a.IO.Out, "  --commit <sha>   Select a pending commit by hash prefix; repeatable\n  --dry-run        Resolve selection and preflight without changing Git refs\n  --json           Supported with --all/--commit, or with --dry-run\n")
			return nil
		}
	}
	if !pickUsesAutomation(global, args) {
		return a.pick(ctx, global, args)
	}
	opts, err := parseAutomatedPick(global, args)
	if err != nil {
		return err
	}
	return a.runAutomatedPick(ctx, opts)
}

func parseAutomatedPick(global globalOptions, args []string) (automatedPickOptions, error) {
	opts := automatedPickOptions{globalOptions: global, source: "work", base: "develop"}
	positionals := make([]string, 0, 1)
	for len(args) > 0 {
		arg := args[0]
		if consumed, err := parseGlobal(&opts.globalOptions, args); err != nil {
			return opts, err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		switch {
		case arg == "--from" || arg == "--base":
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				return opts, clierror.New(clierror.Usage, "%s requires a branch", arg)
			}
			if arg == "--from" {
				opts.source, opts.sourceSet = args[1], true
			} else {
				opts.base, opts.baseSet = args[1], true
			}
			args = args[2:]
		case strings.HasPrefix(arg, "--from="):
			opts.source, opts.sourceSet = strings.TrimPrefix(arg, "--from="), true
			args = args[1:]
		case strings.HasPrefix(arg, "--base="):
			opts.base, opts.baseSet = strings.TrimPrefix(arg, "--base="), true
			args = args[1:]
		case arg == "--fetch":
			opts.fetch, args = true, args[1:]
		case arg == "--allow-stale":
			opts.allowStale, args = true, args[1:]
		case arg == "--all":
			opts.all, args = true, args[1:]
		case arg == "--commit":
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				return opts, clierror.New(clierror.Usage, "--commit requires a commit hash")
			}
			opts.commits = append(opts.commits, args[1])
			args = args[2:]
		case strings.HasPrefix(arg, "--commit="):
			value := strings.TrimSpace(strings.TrimPrefix(arg, "--commit="))
			if value == "" {
				return opts, clierror.New(clierror.Usage, "--commit requires a commit hash")
			}
			opts.commits = append(opts.commits, value)
			args = args[1:]
		case arg == "--dry-run":
			opts.dryRun, args = true, args[1:]
		case arg == "--submit" || arg == "--wait" || arg == "--no-wait":
			opts.submit, args = true, args[1:]
		case arg == "--local":
			opts.localOnly, args = true, args[1:]
		case arg == "--continue" || arg == "--skip" || arg == "--abort":
			return opts, clierror.New(clierror.Usage, "%s is not supported with automated pick flags", arg)
		case strings.HasPrefix(arg, "-"):
			return opts, clierror.New(clierror.Usage, "unknown pick option %q", arg)
		default:
			positionals = append(positionals, arg)
			args = args[1:]
		}
	}
	if len(positionals) != 1 {
		return opts, clierror.New(clierror.Usage, "pick requires exactly one new branch name")
	}
	opts.target = positionals[0]
	if opts.source == "" || opts.base == "" {
		return opts, clierror.New(clierror.Usage, "source and base branch must not be empty")
	}
	if opts.all && len(opts.commits) > 0 {
		return opts, clierror.New(clierror.Usage, "pick --all cannot be combined with --commit")
	}
	if opts.localOnly && opts.submit {
		return opts, clierror.New(clierror.Usage, "--local cannot be combined with --submit, --wait, or --no-wait")
	}
	if !opts.localOnly {
		opts.submit = true
	}
	if opts.dryRun && opts.fetch {
		return opts, clierror.New(clierror.Usage, "--dry-run cannot be combined with --fetch because fetch updates remote-tracking refs")
	}
	if opts.json && !opts.all && len(opts.commits) == 0 {
		return opts, clierror.New(clierror.Usage, "pick --json requires --all or at least one --commit")
	}
	if opts.json && !opts.dryRun && !opts.yes {
		return opts, clierror.New(clierror.Usage, "pick --json mutation requires --yes")
	}
	return opts, nil
}

func (a *Application) runAutomatedPick(ctx context.Context, opts automatedPickOptions) error {
	s, err := a.validatedGit(ctx, opts.cwd)
	if err != nil {
		return err
	}
	c := s.WorkflowConfig(ctx)
	if !opts.baseSet {
		opts.base = c.Base
	}
	if !opts.sourceSet {
		opts.source = c.Source
	}
	if opts.submit && ((opts.sourceSet && opts.source != c.Source) || (opts.baseSet && opts.base != c.Base)) {
		return clierror.New(clierror.Usage, "custom --from or --base can only create a local branch; add --local or update repository config first")
	}
	if err := s.ValidateBranchName(ctx, opts.target); err != nil {
		return clierror.Wrap(clierror.Failure, err, "invalid target branch name %q", opts.target)
	}
	if exists, err := s.LocalBranchExists(ctx, opts.target); err != nil {
		return clierror.Wrap(clierror.Failure, err, "check target branch %q", opts.target)
	} else if exists {
		return clierror.New(clierror.Failure, "target branch %q already exists", opts.target)
	}
	clean, err := s.IsClean(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check working tree")
	}
	if !clean {
		return clierror.New(clierror.Failure, "working tree has changes; commit or stash them before running kit pick")
	}
	if !opts.dryRun && (opts.fetch || opts.submit) {
		if err := s.Fetch(ctx, c.Remote); err != nil {
			return clierror.Wrap(clierror.Failure, err, "fetch %s", c.Remote)
		}
	}
	if err := verifyRevisions(ctx, s, opts.base, opts.source); err != nil {
		return err
	}
	excludedMerges, err := s.MergeCount(ctx, opts.base, opts.source)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "count excluded work merges")
	}
	needsSync, err := pickNeedsSync(ctx, s, c.Remote, opts.base, opts.source)
	if err != nil {
		return err
	}
	if needsSync && !opts.dryRun && !opts.allowStale {
		if opts.json {
			return clierror.New(clierror.Conflict, "repository requires synchronization; run kit sync before JSON pick")
		}
		if opts.sourceSet || opts.baseSet {
			return clierror.New(clierror.Conflict, "%s and %s are not synchronized; run kit sync before pick", opts.source, opts.base)
		}
		if err := a.syncCommand(ctx, opts.globalOptions, nil); err != nil {
			return clierror.Wrap(clierror.Code(err), err, "automatic sync before pick failed")
		}
		needsSync, err = pickNeedsSync(ctx, s, c.Remote, opts.base, opts.source)
		if err != nil {
			return err
		}
		if needsSync {
			return clierror.New(clierror.Conflict, "repository still requires synchronization after kit sync")
		}
	}
	if remoteExists, err := s.RemoteTrackingBranchExists(ctx, c.Remote, opts.target); err != nil {
		return clierror.Wrap(clierror.Failure, err, "check remote-tracking branch %s/%s", c.Remote, opts.target)
	} else if remoteExists {
		return clierror.New(clierror.Failure, "target branch %q already exists on %s", opts.target, c.Remote)
	}
	commits, err := s.Candidates(ctx, opts.base, opts.source, true)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "list commits from %s", opts.source)
	}
	commits, err = s.Applied(ctx, opts.base, commits)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "filter already applied commits")
	}
	pending := make([]gitservice.Commit, 0, len(commits))
	for _, commit := range commits {
		if !commit.Applied {
			pending = append(pending, commit)
		}
	}
	selected, err := a.selectPendingCommits(pending, opts.commits, opts.all, fmt.Sprintf("%s → %s (%s에서 새 브랜치)", opts.source, opts.target, opts.base))
	if err != nil {
		if errors.Is(err, selector.ErrCanceled) {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
		if errors.Is(err, selector.ErrInterrupted) {
			return clierror.New(clierror.Interrupt, "interrupted")
		}
		if errors.Is(err, selector.ErrNotTTY) {
			return clierror.New(clierror.Failure, "automated pick requires --all or --commit when no TTY is available")
		}
		return err
	}
	plan := automatedPickPlan{
		Target: opts.target, Source: opts.source, Base: opts.base, Commits: selected,
		NeedsSync: needsSync, WillPush: opts.submit, WillCreatePR: opts.submit, DryRun: opts.dryRun,
	}
	if opts.dryRun {
		if opts.json {
			return writeJSON(a.IO.Out, plan)
		}
		a.printPickSummary(pickOptions{globalOptions: opts.globalOptions, target: opts.target, source: opts.source, base: opts.base, all: opts.all, submit: opts.submit, excludedMerges: excludedMerges}, selected)
		r := a.renderer(opts.globalOptions)
		r.Notice("Dry run")
		r.Field("Mutation", "Git ref, checkout, remote를 변경하지 않았습니다.")
		if needsSync {
			r.Warning("Sync", "실행 전에 kit sync가 필요합니다.")
		}
		return nil
	}
	if len(selected) == 0 {
		if opts.json {
			return writeJSON(a.IO.Out, automatedLocalPickResult{Branch: opts.target})
		}
		r := a.renderer(opts.globalOptions)
		r.Command("pick")
		r.Success("Work", "선택할 미반영 커밋이 없습니다.")
		return nil
	}
	if !opts.json {
		a.printPickSummary(pickOptions{globalOptions: opts.globalOptions, target: opts.target, source: opts.source, base: opts.base, all: opts.all, submit: opts.submit, excludedMerges: excludedMerges}, selected)
	}
	if !opts.yes {
		ok, confirmErr := confirm(a.IO.In, a.IO.Out, fmt.Sprintf("선택한 %d개 commit으로 %s를 만들%s? [y/N] ", len(selected), opts.target, map[bool]string{true: "고 push·PR을 생성하시겠습니까", false: "겠습니까"}[opts.submit]))
		if confirmErr != nil {
			return confirmErr
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}
	if existing, loadErr := pickstate.Load(ctx, s); loadErr == nil {
		return clierror.New(clierror.Conflict, "kit pick %s is already in progress; resume it first", existing.TargetBranch)
	}
	originalHash, originalBranch, err := s.Head(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "record current checkout")
	}
	state := pickstate.State{OriginalHash: originalHash, OriginalBranch: originalBranch, TargetBranch: opts.target, BaseBranch: opts.base, SourceBranch: opts.source, SubmitAfterPick: opts.submit}
	for _, commit := range selected {
		state.Commits = append(state.Commits, commit.Hash)
	}
	if err := pickstate.Save(ctx, s, state); err != nil {
		return clierror.Wrap(clierror.Failure, err, "save pick state")
	}
	if err := s.CreateBranch(ctx, opts.target, opts.base); err != nil {
		_ = pickstate.Remove(ctx, s)
		return clierror.Wrap(clierror.Failure, err, "create branch %q from %q", opts.target, opts.base)
	}
	if err := s.MarkKitCreatedBranch(ctx, opts.target); err != nil {
		_ = s.RestoreAndDeleteBranch(ctx, originalHash, originalBranch, opts.target)
		_ = pickstate.Remove(ctx, s)
		return clierror.Wrap(clierror.Failure, err, "mark Kit-created branch %q", opts.target)
	}
	if opts.json {
		return a.runJSONPick(ctx, opts.globalOptions, s, state)
	}
	return a.runPick(ctx, opts.globalOptions, s, state, bufio.NewReader(a.IO.In))
}

func pickNeedsSync(ctx context.Context, s gitservice.Service, remote, base, source string) (bool, error) {
	synced, err := s.IsAncestor(ctx, base, source)
	if err != nil {
		return false, clierror.Wrap(clierror.Failure, err, "check whether %s contains %s", source, base)
	}
	needsSync := !synced
	remoteBase := remote + "/" + base
	if err := s.VerifyRevision(ctx, remoteBase); err == nil {
		ahead, behind, compareErr := s.AheadBehind(ctx, base, remoteBase)
		if compareErr != nil {
			return false, clierror.Wrap(clierror.Failure, compareErr, "compare %s with %s", base, remoteBase)
		}
		needsSync = needsSync || ahead != 0 || behind != 0
	}
	return needsSync, nil
}

func (a *Application) runJSONPick(ctx context.Context, global globalOptions, s gitservice.Service, state pickstate.State) error {
	for state.Next < len(state.Commits) {
		commit, err := s.Commit(ctx, state.Commits[state.Next])
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "read next pick commit")
		}
		state.HeadBefore, err = s.RevisionHash(ctx, "HEAD")
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "record branch before cherry-pick")
		}
		if err := pickstate.Save(ctx, s, state); err != nil {
			return clierror.Wrap(clierror.Failure, err, "save pick checkpoint")
		}
		if err := s.CherryPickOne(ctx, commit.Hash); err != nil {
			return clierror.Wrap(clierror.Conflict, err, "pick paused at %s; resolve and run kit pick --continue/--skip/--abort", commit.ShortHash)
		}
		state.Next++
		state.HeadBefore = ""
		if err := pickstate.Save(ctx, s, state); err != nil {
			return clierror.Wrap(clierror.Failure, err, "save pick progress")
		}
	}
	if err := pickstate.Remove(ctx, s); err != nil {
		return clierror.Wrap(clierror.Failure, err, "remove completed pick state")
	}
	if state.SubmitAfterPick {
		pushAttempted := false
		err := a.reviewSubmit(ctx, global, reviewSubmitOptions{removeSourceBranch: true, confirmed: true, pushAttempted: &pushAttempted, embedded: true}, &s)
		if err == nil {
			return nil
		}
		if pushAttempted {
			return clierror.Wrap(clierror.Code(err), err, "review submit did not finish after push started; branch %s was kept", state.TargetBranch)
		}
		if rollbackErr := rollbackPickBeforePush(s, state); rollbackErr != nil {
			return clierror.Wrap(clierror.Code(err), err, "review submit failed before push and rollback was incomplete: %v", rollbackErr)
		}
		return clierror.Wrap(clierror.Code(err), err, "review submit failed before push; restored original checkout")
	}
	return writeJSON(a.IO.Out, automatedLocalPickResult{Branch: state.TargetBranch, Commits: append([]string(nil), state.Commits...)})
}

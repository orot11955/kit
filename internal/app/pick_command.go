package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/pickstate"
	"kit/internal/selector"
	"kit/internal/ui"
)

type pickOptions struct {
	globalOptions
	target         string
	source         string
	base           string
	sourceSet      bool
	baseSet        bool
	fetch          bool
	allowStale     bool
	all            bool
	excludedMerges int
	submit         bool
	localOnly      bool
	action         string
}

func parsePick(global globalOptions, args []string) (pickOptions, bool, error) {
	opts := pickOptions{globalOptions: global, source: "work", base: "develop"}
	positionals := []string{}
	for len(args) > 0 {
		arg := args[0]
		if arg == "-h" || arg == "--help" {
			return opts, true, nil
		}
		if consumed, err := parseGlobal(&opts.globalOptions, args); err != nil {
			return opts, false, err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		switch {
		case arg == "--from" || arg == "--base":
			if len(args) < 2 {
				return opts, false, clierror.New(clierror.Usage, "%s requires a branch", arg)
			}
			if arg == "--from" {
				opts.source, opts.sourceSet = args[1], true
			} else {
				opts.base, opts.baseSet = args[1], true
			}
			args = args[2:]
		case strings.HasPrefix(arg, "--from="):
			opts.source, opts.sourceSet, args = strings.TrimPrefix(arg, "--from="), true, args[1:]
		case strings.HasPrefix(arg, "--base="):
			opts.base, opts.baseSet, args = strings.TrimPrefix(arg, "--base="), true, args[1:]
		case arg == "--fetch":
			opts.fetch, args = true, args[1:]
		case arg == "--allow-stale":
			opts.allowStale, args = true, args[1:]
		case arg == "--all":
			opts.all, args = true, args[1:]
		case arg == "--submit":
			opts.submit, args = true, args[1:]
		case arg == "--local":
			opts.localOnly, args = true, args[1:]
		case arg == "--wait" || arg == "--no-wait":
			// Accepted for one release; Create always returns immediately.
			opts.submit, args = true, args[1:]
		case arg == "--continue" || arg == "--skip" || arg == "--abort":
			if opts.action != "" {
				return opts, false, clierror.New(clierror.Usage, "pick accepts only one resume action")
			}
			opts.action, args = strings.TrimPrefix(arg, "--"), args[1:]
		case strings.HasPrefix(arg, "-"):
			return opts, false, clierror.New(clierror.Usage, "unknown pick option %q", arg)
		default:
			positionals, args = append(positionals, arg), args[1:]
		}
	}
	if opts.action != "" && len(positionals) != 0 {
		return opts, false, clierror.New(clierror.Usage, "pick resume actions do not accept a new branch name")
	}
	if opts.action != "" && (opts.submit || opts.localOnly) {
		return opts, false, clierror.New(clierror.Usage, "pick resume actions use the submit settings saved by the original pick")
	}
	if opts.action != "" && opts.all {
		return opts, false, clierror.New(clierror.Usage, "pick resume actions use the commit selection saved by the original pick")
	}
	if opts.localOnly && opts.submit {
		return opts, false, clierror.New(clierror.Usage, "--local cannot be combined with --submit, --wait, or --no-wait")
	}
	if opts.action == "" && len(positionals) != 1 {
		return opts, false, clierror.New(clierror.Usage, "pick requires exactly one new branch name")
	}
	if len(positionals) == 1 {
		opts.target = positionals[0]
	}
	if opts.action == "" && !opts.localOnly {
		opts.submit = true
	}
	if opts.source == "" || opts.base == "" {
		return opts, false, clierror.New(clierror.Usage, "source and base branch must not be empty")
	}
	if opts.json {
		return opts, false, clierror.New(clierror.Usage, "--json is not supported by interactive pick")
	}
	return opts, false, nil
}

func (a *Application) pick(ctx context.Context, global globalOptions, args []string) error {
	opts, help, err := parsePick(global, args)
	if err != nil {
		return err
	}
	if help {
		printPickHelp(a.IO.Out)
		return nil
	}
	service, err := a.validatedGit(ctx, opts.cwd)
	if err != nil {
		return err
	}
	if opts.action != "" {
		return a.resumePick(ctx, opts.globalOptions, service, opts.action)
	}
	config := service.WorkflowConfig(ctx)
	if !opts.baseSet {
		opts.base = config.Base
	}
	if !opts.sourceSet {
		opts.source = config.Source
	}
	if opts.submit && ((opts.sourceSet && opts.source != config.Source) || (opts.baseSet && opts.base != config.Base)) {
		return clierror.New(clierror.Usage, "custom --from or --base can only create a local branch; add --local or update the repository config first")
	}
	if err := service.ValidateBranchName(ctx, opts.target); err != nil {
		return clierror.Wrap(clierror.Failure, err, "invalid target branch name %q", opts.target)
	}
	exists, err := service.LocalBranchExists(ctx, opts.target)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check target branch %q", opts.target)
	}
	if exists {
		return clierror.New(clierror.Failure, "target branch %q already exists", opts.target)
	}
	clean, err := service.IsClean(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check working tree")
	}
	if !clean {
		return clierror.New(clierror.Failure, "working tree has changes; commit or stash them before running kit pick")
	}
	if opts.fetch || opts.submit {
		if err := service.Fetch(ctx, config.Remote); err != nil {
			return clierror.Wrap(clierror.Failure, err, "fetch %s", config.Remote)
		}
	}
	if err := verifyRevisions(ctx, service, opts.base, opts.source); err != nil {
		return err
	}
	opts.excludedMerges, err = service.MergeCount(ctx, opts.base, opts.source)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "count excluded work merges")
	}
	synced, err := service.IsAncestor(ctx, opts.base, opts.source)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check whether %s contains %s", opts.source, opts.base)
	}
	needsSync := !synced
	remoteBase := config.Remote + "/" + opts.base
	if err := service.VerifyRevision(ctx, remoteBase); err == nil {
		ahead, behind, compareErr := service.AheadBehind(ctx, opts.base, remoteBase)
		if compareErr != nil {
			return clierror.Wrap(clierror.Failure, compareErr, "compare %s with %s", opts.base, remoteBase)
		}
		needsSync = needsSync || ahead > 0 || behind > 0
	}
	if needsSync && !opts.allowStale {
		if opts.sourceSet || opts.baseSet {
			return clierror.New(clierror.Conflict, "%s, %s, and %s are not synchronized; run 'kit sync' before pick (use --allow-stale only for recovery)", opts.source, opts.base, remoteBase)
		}
		if err := a.syncCommand(ctx, opts.globalOptions, nil); err != nil {
			return clierror.Wrap(clierror.Code(err), err, "automatic sync before pick failed")
		}
		synced, err = service.IsAncestor(ctx, opts.base, opts.source)
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "verify automatic sync before pick")
		}
		if !synced {
			return clierror.New(clierror.Conflict, "%s still does not contain %s after sync", opts.source, opts.base)
		}
		if err := service.VerifyRevision(ctx, remoteBase); err == nil {
			ahead, behind, compareErr := service.AheadBehind(ctx, opts.base, remoteBase)
			if compareErr != nil {
				return clierror.Wrap(clierror.Failure, compareErr, "verify base after automatic sync")
			}
			if ahead != 0 || behind != 0 {
				return clierror.New(clierror.Conflict, "%s still differs from %s after sync", opts.base, remoteBase)
			}
		}
	}
	remoteExists, err := service.RemoteTrackingBranchExists(ctx, config.Remote, opts.target)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check remote-tracking branch %s/%s", config.Remote, opts.target)
	}
	if remoteExists {
		return clierror.New(clierror.Failure, "target branch %q already exists on %s; fetch/prune or choose another name", opts.target, config.Remote)
	}
	commits, err := service.Candidates(ctx, opts.base, opts.source, true)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "list commits from %s", opts.source)
	}
	commits, err = service.Applied(ctx, opts.base, commits)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "filter already applied commits")
	}
	pending := commits[:0]
	for _, commit := range commits {
		if !commit.Applied {
			pending = append(pending, commit)
		}
	}
	if len(pending) == 0 {
		renderer := a.renderer(opts.globalOptions)
		renderer.Command("pick")
		renderer.Success("Work", "미반영 커밋이 없습니다.")
		return nil
	}
	selected := pending
	if !opts.all {
		items := make([]selector.Item, 0, len(pending))
		byHash := make(map[string]gitservice.Commit, len(pending))
		for _, commit := range pending {
			subject := ui.SafeText(commit.Subject)
			items = append(items, selector.Item{ID: commit.Hash, Display: fmt.Sprintf("%-10s  %-16s  %s", commit.ShortHash, commit.Date, subject), Search: commit.ShortHash + " " + commit.Date + " " + subject})
			byHash[commit.Hash] = commit
		}
		if a.Select == nil {
			return clierror.New(clierror.Failure, "interactive selection is unavailable")
		}
		selectedItems, selectErr := a.Select(items, fmt.Sprintf("%s → %s (%s에서 새 브랜치)", opts.source, opts.target, opts.base))
		if errors.Is(selectErr, selector.ErrCanceled) {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
		if errors.Is(selectErr, selector.ErrInterrupted) {
			return clierror.New(clierror.Interrupt, "interrupted")
		}
		if errors.Is(selectErr, selector.ErrNotTTY) {
			return clierror.New(clierror.Failure, "kit pick requires an interactive TTY; use --all to select every pending commit")
		}
		if selectErr != nil {
			return clierror.Wrap(clierror.Failure, selectErr, "open commit selector")
		}
		if len(selectedItems) == 0 {
			fmt.Fprintln(a.IO.Out, "선택한 커밋이 없어 취소되었습니다.")
			return nil
		}
		selected = make([]gitservice.Commit, 0, len(selectedItems))
		for _, item := range selectedItems {
			selected = append(selected, byHash[item.ID])
		}
	}
	a.printPickSummary(opts, selected)
	reader := bufio.NewReader(a.IO.In)
	if !opts.yes {
		prompt := "선택한 커밋으로 로컬 브랜치를 만드시겠습니까? [y/N] "
		if opts.submit {
			prompt = "선택한 커밋으로 브랜치를 만들고 push·Gitea PR을 생성하시겠습니까? [y/N] "
		}
		fmt.Fprint(a.IO.Out, prompt)
		answer, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return clierror.Wrap(clierror.Failure, readErr, "read confirmation")
		}
		answer = strings.TrimSpace(answer)
		if answer != "y" && answer != "Y" {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}
	if existing, loadErr := pickstate.Load(ctx, service); loadErr == nil {
		return clierror.New(clierror.Conflict, "kit pick %s is already in progress; resume it before starting another pick", existing.TargetBranch)
	}
	originalHash, originalBranch, err := service.Head(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "record current checkout")
	}
	state := pickstate.State{
		OriginalHash: originalHash, OriginalBranch: originalBranch, TargetBranch: opts.target,
		BaseBranch: opts.base, SourceBranch: opts.source,
		SubmitAfterPick: opts.submit,
	}
	for _, commit := range selected {
		state.Commits = append(state.Commits, commit.Hash)
	}
	if err := pickstate.Save(ctx, service, state); err != nil {
		return clierror.Wrap(clierror.Failure, err, "save pick state")
	}
	if err := service.CreateBranch(ctx, opts.target, opts.base); err != nil {
		_ = pickstate.Remove(ctx, service)
		return clierror.Wrap(clierror.Failure, err, "create branch %q from %q", opts.target, opts.base)
	}
	if err := service.MarkKitCreatedBranch(ctx, opts.target); err != nil {
		_ = service.RestoreAndDeleteBranch(ctx, originalHash, originalBranch, opts.target)
		_ = pickstate.Remove(ctx, service)
		return clierror.Wrap(clierror.Failure, err, "mark Kit-created branch %q", opts.target)
	}
	return a.runPick(ctx, opts.globalOptions, service, state, reader)
}

func (a *Application) runPick(ctx context.Context, global globalOptions, service gitservice.Service, state pickstate.State, reader *bufio.Reader) error {
	return a.runPickLocked(ctx, global, service, state, reader)
}

func (a *Application) runPickLocked(ctx context.Context, global globalOptions, service gitservice.Service, state pickstate.State, reader *bufio.Reader) error {
	for {
		if state.Next >= len(state.Commits) {
			_, err := service.RevisionHash(ctx, state.TargetBranch)
			if err != nil {
				return clierror.Wrap(clierror.Failure, err, "read completed review branch")
			}
			if err := pickstate.Remove(ctx, service); err != nil {
				return clierror.Wrap(clierror.Failure, err, "remove completed pick state")
			}
			if state.SubmitAfterPick {
				pushAttempted := false
				err := a.reviewSubmit(ctx, global, reviewSubmitOptions{
					removeSourceBranch: true, confirmed: true,
					pushAttempted: &pushAttempted, embedded: true,
				}, &service)
				if err == nil {
					return nil
				}
				if pushAttempted {
					return clierror.Wrap(clierror.Code(err), err, "review submit did not finish after push started; local and remote branch %s were kept; verify the PR manually", state.TargetBranch)
				}
				if rollbackErr := rollbackPickBeforePush(service, state); rollbackErr != nil {
					return clierror.Wrap(clierror.Code(err), err, "review submit failed before push; automatic rollback was incomplete: %v", rollbackErr)
				}
				return clierror.Wrap(clierror.Code(err), err, "review submit failed before push; restored %s and removed %s", state.OriginalBranch, state.TargetBranch)
			}
			renderer := a.renderer(global)
			renderer.Section("준비")
			renderer.Success("Branch", fmt.Sprintf("%s · %d개 커밋", state.TargetBranch, len(state.Commits)))
			renderer.Next("kit review submit " + ui.ShellQuote(state.TargetBranch))
			return nil
		}
		commit, err := service.Commit(ctx, state.Commits[state.Next])
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "read next pick commit")
		}
		state.HeadBefore, err = service.RevisionHash(ctx, "HEAD")
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "record branch before cherry-pick")
		}
		if err := pickstate.Save(ctx, service, state); err != nil {
			return clierror.Wrap(clierror.Failure, err, "save pick checkpoint")
		}
		if err := service.CherryPickOne(ctx, commit.Hash); err == nil {
			state.Next++
			state.HeadBefore = ""
			if err := pickstate.Save(ctx, service, state); err != nil {
				return clierror.Wrap(clierror.Failure, err, "save pick progress")
			}
			continue
		}
		fmt.Fprintf(a.IO.Out, "\n%s (%s)를 적용할 수 없습니다.\n", commit.ShortHash, ui.SafeText(commit.Subject))
		if status := service.StatusShort(ctx); status != "" {
			fmt.Fprintln(a.IO.Out, status)
		}
		fmt.Fprint(a.IO.Out, "충돌을 해결하고 필요한 파일을 git add한 뒤 [c]ontinue, [s]kip, [a]bort, [q]uit: ")
		action, readErr := reader.ReadString('\n')
		if readErr != nil {
			return clierror.New(clierror.Conflict, "pick is paused; resolve it and run 'kit pick --continue', '--skip', or '--abort'")
		}
		switch strings.ToLower(strings.TrimSpace(action)) {
		case "c", "continue":
			if err := a.advancePick(ctx, service, &state, "continue"); err != nil {
				fmt.Fprintf(a.IO.Out, "%v\n", err)
				continue
			}
		case "s", "skip":
			if err := a.advancePick(ctx, service, &state, "skip"); err != nil {
				fmt.Fprintf(a.IO.Out, "%v\n", err)
				continue
			}
		case "a", "abort":
			return a.abortPick(ctx, service, state)
		case "q", "quit":
			return clierror.New(clierror.Conflict, "pick is paused; resume with 'kit pick --continue', '--skip', or '--abort'")
		default:
			fmt.Fprintln(a.IO.Out, "c, s, a, q 중 하나를 선택하세요.")
		}
	}
}

func rollbackPickBeforePush(service gitservice.Service, state pickstate.State) error {
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.RestoreAndDeleteBranch(rollbackCtx, state.OriginalHash, state.OriginalBranch, state.TargetBranch); err != nil {
		return err
	}
	_ = service.ClearKitCreatedBranch(rollbackCtx, state.TargetBranch)
	headHash, headBranch, err := service.Head(rollbackCtx)
	if err != nil {
		return fmt.Errorf("verify original checkout: %w", err)
	}
	if headHash != state.OriginalHash || headBranch != state.OriginalBranch {
		return fmt.Errorf("verify original checkout: got branch %q at %s, want branch %q at %s", headBranch, headHash, state.OriginalBranch, state.OriginalHash)
	}
	exists, err := service.LocalBranchExists(rollbackCtx, state.TargetBranch)
	if err != nil {
		return fmt.Errorf("verify target branch removal: %w", err)
	}
	if exists {
		return fmt.Errorf("verify target branch removal: %s still exists", state.TargetBranch)
	}
	return nil
}

func (a *Application) resumePick(ctx context.Context, global globalOptions, service gitservice.Service, action string) error {
	state, err := pickstate.Load(ctx, service)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "load pick state")
	}
	if action == "abort" {
		return a.abortPick(ctx, service, state)
	}
	if state.Next >= len(state.Commits) {
		return a.runPickLocked(ctx, global, service, state, bufio.NewReader(a.IO.In))
	}
	if err := a.advancePick(ctx, service, &state, action); err != nil {
		return err
	}
	return a.runPickLocked(ctx, global, service, state, bufio.NewReader(a.IO.In))
}

func (a *Application) advancePick(ctx context.Context, service gitservice.Service, state *pickstate.State, action string) error {
	inProgress, err := service.CherryPickInProgress(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Conflict, err, "check cherry-pick state")
	}
	if !inProgress {
		currentHead, err := service.RevisionHash(ctx, "HEAD")
		if err != nil {
			return clierror.Wrap(clierror.Conflict, err, "read branch after external conflict resolution")
		}
		if state.HeadBefore != "" && currentHead != state.HeadBefore {
			if action == "skip" {
				return clierror.New(clierror.Conflict, "conflict resolution was already committed; use --continue to keep it or --abort to discard the review branch")
			}
			if action != "continue" {
				return clierror.New(clierror.Usage, "unknown pick action %q", action)
			}
			state.Next++
			state.HeadBefore = ""
			if err := pickstate.Save(ctx, service, *state); err != nil {
				return clierror.Wrap(clierror.Failure, err, "save externally resolved pick progress")
			}
			return nil
		}
		return clierror.New(clierror.Conflict, "cherry-pick state is missing and no external resolution commit was detected; use --abort to restore the original checkout")
	}
	if action == "continue" {
		unresolved, err := service.Unresolved(ctx)
		if err != nil {
			return clierror.Wrap(clierror.Conflict, err, "check unresolved files")
		}
		if len(unresolved) > 0 {
			return clierror.New(clierror.Conflict, "unresolved files remain: %s", strings.Join(unresolved, ", "))
		}
		if err := service.Continue(ctx); err != nil {
			return clierror.Wrap(clierror.Conflict, err, "continue cherry-pick; stage resolved files with git add first")
		}
	} else if action == "skip" {
		if err := service.Skip(ctx); err != nil {
			return clierror.Wrap(clierror.Conflict, err, "skip cherry-pick")
		}
	} else {
		return clierror.New(clierror.Usage, "unknown pick action %q", action)
	}
	state.Next++
	state.HeadBefore = ""
	if err := pickstate.Save(ctx, service, *state); err != nil {
		return clierror.Wrap(clierror.Failure, err, "save pick progress")
	}
	return nil
}

func (a *Application) abortPick(ctx context.Context, service gitservice.Service, state pickstate.State) error {
	inProgress, err := service.CherryPickInProgress(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Conflict, err, "check cherry-pick state before abort")
	}
	if inProgress {
		if err := service.Abort(ctx); err != nil {
			return clierror.Wrap(clierror.Conflict, err, "abort cherry-pick")
		}
	}
	if err := service.RestoreAndDeleteBranch(ctx, state.OriginalHash, state.OriginalBranch, state.TargetBranch); err != nil {
		return clierror.Wrap(clierror.Failure, err, "cherry-pick was aborted, but target branch cleanup failed")
	}
	_ = service.ClearKitCreatedBranch(ctx, state.TargetBranch)
	if err := pickstate.Remove(ctx, service); err != nil {
		return clierror.Wrap(clierror.Failure, err, "remove pick state")
	}
	return clierror.New(clierror.Failure, "cherry-pick aborted; restored the original checkout and removed %q", state.TargetBranch)
}

func (a *Application) printPickSummary(opts pickOptions, selected []gitservice.Commit) {
	renderer := a.renderer(opts.globalOptions)
	renderer.Command("pick")
	renderer.Section("준비")
	renderer.Field("New", opts.target)
	renderer.Field("Base", opts.base)
	renderer.Field("Work", opts.source)
	if opts.excludedMerges > 0 {
		renderer.Warning("Excluded", fmt.Sprintf("work merge %d개와 side-parent 경로는 선택 후보에 포함되지 않습니다", opts.excludedMerges))
	}
	if opts.submit {
		renderer.Field("Action", "브랜치 생성 · push · Gitea PR 생성")
	} else {
		renderer.Field("Action", "로컬 브랜치 생성")
	}
	renderer.Section(fmt.Sprintf("선택한 커밋 · %d개", len(selected)))
	displayed := selected
	const compactLimit = 20
	const compactTail = 3
	if opts.all && len(selected) > compactLimit {
		displayed = selected[:compactLimit-compactTail]
	}
	for _, commit := range displayed {
		renderer.Pending(commit.ShortHash, commit.Subject)
	}
	if len(displayed) != len(selected) {
		renderer.Field("…", fmt.Sprintf("%d개 생략", len(selected)-len(displayed)-compactTail))
		for _, commit := range selected[len(selected)-compactTail:] {
			renderer.Pending(commit.ShortHash, commit.Subject)
		}
	}
	fmt.Fprintln(a.IO.Out)
}

func printPickHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: kit pick <new-branch> [options]

Options:
  --from <branch>  Source branch; custom values require --local (default: work)
  --base <branch>  Base branch; custom values require --local (default: develop)
  --fetch          Fetch the configured remote before selection
  --allow-stale    Allow an outdated source branch (recovery only)
  --all            Select every pending commit without opening the selector
  --local          Create the local branch without push or PR creation
  --no-wait        Deprecated compatibility option; Create always returns immediately
  --wait           Deprecated compatibility option; Create always returns immediately
  --submit         Compatibility option; submission is the default
  --continue       Continue a paused kit pick after staging resolutions
  --skip           Skip the current commit in a paused kit pick
  --abort          Abort a paused kit pick and restore the original checkout
  --cwd <path>     Git repository directory
  --yes            Skip confirmation after commit selection
  --no-color       Disable ANSI colors outside the selector
`)
}

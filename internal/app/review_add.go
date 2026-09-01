package app

import (
	"context"
	"fmt"
	"strings"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/review"
	"kit/internal/reviewstate"
	"kit/internal/selector"
	"kit/internal/ui"
)

type reviewAddOptions struct {
	globalOptions
	branch  string
	all     bool
	commits []string
}

type reviewAddResult struct {
	Branch       string              `json:"branch"`
	ReviewNumber int64               `json:"review_number"`
	ReviewURL    string              `json:"review_url"`
	Added        []gitservice.Commit `json:"added"`
	PublishedTip string              `json:"published_tip"`
}

func parseReviewAdd(global globalOptions, args []string) (reviewAddOptions, bool, error) {
	opts := reviewAddOptions{globalOptions: global}
	positionals := make([]string, 0, 1)
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
		case arg == "--all":
			opts.all, args = true, args[1:]
		case arg == "--commit":
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				return opts, false, clierror.New(clierror.Usage, "--commit requires a commit hash")
			}
			opts.commits = append(opts.commits, args[1])
			args = args[2:]
		case strings.HasPrefix(arg, "--commit="):
			value := strings.TrimSpace(strings.TrimPrefix(arg, "--commit="))
			if value == "" {
				return opts, false, clierror.New(clierror.Usage, "--commit requires a commit hash")
			}
			opts.commits = append(opts.commits, value)
			args = args[1:]
		case strings.HasPrefix(arg, "-"):
			return opts, false, clierror.New(clierror.Usage, "unknown review add option %q", arg)
		default:
			positionals = append(positionals, arg)
			args = args[1:]
		}
	}
	if len(positionals) > 1 {
		return opts, false, clierror.New(clierror.Usage, "review add accepts at most one review branch")
	}
	if len(positionals) == 1 {
		opts.branch = positionals[0]
	}
	if opts.all && len(opts.commits) > 0 {
		return opts, false, clierror.New(clierror.Usage, "review add --all cannot be combined with --commit")
	}
	if opts.json && !opts.all && len(opts.commits) == 0 {
		return opts, false, clierror.New(clierror.Usage, "review add --json requires --all or at least one --commit")
	}
	if opts.json && !opts.yes {
		return opts, false, clierror.New(clierror.Usage, "review add --json requires --yes")
	}
	return opts, false, nil
}

func (a *Application) reviewAdd(ctx context.Context, global globalOptions, args []string) error {
	opts, help, err := parseReviewAdd(global, args)
	if err != nil {
		return err
	}
	if help {
		fmt.Fprint(a.IO.Out, "Usage: kit review add [branch] [--commit <sha> ... | --all] [--yes] [--json]\n")
		return nil
	}
	s, err := a.reviewGitService(ctx, opts.globalOptions, nil)
	if err != nil {
		return err
	}
	c := s.WorkflowConfig(ctx)
	pushRemote := c.PushRemoteName()
	branch, err := resolveReviewBranch(ctx, s, opts.branch)
	if err != nil {
		return err
	}
	state, err := reviewstate.Load(ctx, s, branch)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "load review state")
	}
	if state.Remote != "" && state.Remote != pushRemote {
		return clierror.New(clierror.Conflict, "review was published to %s, but configured push remote is %s", state.Remote, pushRemote)
	}
	state, refreshed, err := a.refreshReviewState(ctx, s, state)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "refresh PR #%d", state.ReviewNumber)
	}
	if !refreshed {
		return clierror.New(clierror.Failure, "provider %q cannot verify the review before adding commits", state.Provider)
	}
	if state.Status != review.StatusOpen || state.Stage != reviewstate.StageOpen {
		return clierror.New(clierror.Conflict, "PR #%d is not open; current status is %s", state.ReviewNumber, state.Status)
	}
	if state.TargetBranch != c.Base {
		return clierror.New(clierror.Conflict, "review targets %s, but configured base is %s", state.TargetBranch, c.Base)
	}
	if state.SourceBranch != "" && state.SourceBranch != c.Source {
		return clierror.New(clierror.Conflict, "review source queue is %s, but configured source is %s", state.SourceBranch, c.Source)
	}
	managed, err := s.IsKitCreatedBranch(ctx, branch)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check Kit-created branch marker")
	}
	if !managed {
		return clierror.New(clierror.Conflict, "review branch %q is not marked as Kit-created; refusing to mutate it", branch)
	}
	clean, err := s.IsClean(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check working tree")
	}
	if !clean {
		return clierror.New(clierror.Conflict, "working tree has changes; commit or stash them before review add")
	}
	if err := s.Fetch(ctx, c.Remote); err != nil {
		return clierror.Wrap(clierror.Failure, err, "fetch %s", c.Remote)
	}
	if pushRemote != c.Remote {
		if err := s.Fetch(ctx, pushRemote); err != nil {
			return clierror.Wrap(clierror.Failure, err, "fetch %s", pushRemote)
		}
	}
	remoteBase := c.Remote + "/" + c.Base
	if err := s.VerifyRevision(ctx, remoteBase); err != nil {
		return clierror.Wrap(clierror.Failure, err, "remote base %s is unavailable", remoteBase)
	}
	ahead, behind, err := s.AheadBehind(ctx, c.Base, remoteBase)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "compare %s with %s", c.Base, remoteBase)
	}
	if ahead != 0 || behind != 0 {
		return clierror.New(clierror.Conflict, "%s differs from %s; run kit sync before review add", c.Base, remoteBase)
	}
	containsBase, err := s.IsAncestor(ctx, c.Base, c.Source)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check source queue synchronization")
	}
	if !containsBase {
		return clierror.New(clierror.Conflict, "%s is stale; run kit sync before review add", c.Source)
	}
	localExists, err := s.LocalBranchExists(ctx, branch)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check local review branch")
	}
	if !localExists {
		return clierror.New(clierror.Conflict, "local review branch %q is missing", branch)
	}
	remoteExists, err := s.RemoteTrackingBranchExists(ctx, pushRemote, branch)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check remote review branch")
	}
	if !remoteExists {
		return clierror.New(clierror.Conflict, "remote review branch %s/%s is missing", pushRemote, branch)
	}
	localTip, err := s.RevisionHash(ctx, branch)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "read local review branch tip")
	}
	remoteTip, err := s.RevisionHash(ctx, pushRemote+"/"+branch)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "read remote review branch tip")
	}
	if state.PublishedTip == "" || !strings.EqualFold(localTip, state.PublishedTip) || !strings.EqualFold(remoteTip, state.PublishedTip) {
		return clierror.New(clierror.Conflict, "review branch tips are not synchronized with saved published tip; run kit review status and inspect %s", branch)
	}

	state, err = mergeReviewSourceCommits(ctx, s, c, state)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "reconcile saved review commit metadata")
	}
	pending, err := reviewAddPending(ctx, s, c, state)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		if opts.json {
			return writeJSON(a.IO.Out, reviewAddResult{Branch: branch, ReviewNumber: state.ReviewNumber, ReviewURL: state.ReviewURL, PublishedTip: state.PublishedTip})
		}
		r := a.renderer(opts.globalOptions)
		r.Command("review add")
		r.Success("Pending", "추가할 work commit이 없습니다.")
		return nil
	}
	selected, err := a.selectPendingCommits(pending, opts.commits, opts.all, fmt.Sprintf("%s PR #%d에 추가", branch, state.ReviewNumber))
	if err != nil {
		if err == selector.ErrCanceled {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
		if err == selector.ErrInterrupted {
			return clierror.New(clierror.Interrupt, "interrupted")
		}
		if err == selector.ErrNotTTY {
			return clierror.New(clierror.Failure, "review add requires an interactive TTY; use --all or --commit")
		}
		return err
	}
	if len(selected) == 0 {
		fmt.Fprintln(a.IO.Out, "선택한 커밋이 없어 취소되었습니다.")
		return nil
	}
	if !opts.json {
		r := a.renderer(opts.globalOptions)
		r.Command("review add")
		r.Field("Review", fmt.Sprintf("#%d · %s", state.ReviewNumber, state.ReviewURL))
		for _, commit := range selected {
			r.Pending(commit.ShortHash, commit.Subject)
		}
	}
	if !opts.yes {
		ok, confirmErr := confirm(a.IO.In, a.IO.Out, fmt.Sprintf("선택한 %d개 commit을 %s에 추가하고 push하시겠습니까? [y/N] ", len(selected), branch))
		if confirmErr != nil {
			return confirmErr
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}

	originalHash, originalBranch, err := s.Head(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "record current checkout")
	}
	if originalBranch != branch {
		if err := s.Switch(ctx, branch); err != nil {
			return clierror.Wrap(clierror.Failure, err, "switch to review branch %s", branch)
		}
	}
	restore := func() error { return restoreReconcileCheckout(ctx, s, originalHash, originalBranch) }
	upstream, err := s.Upstream(ctx)
	if err != nil {
		_ = restore()
		return clierror.Wrap(clierror.Failure, err, "read review branch upstream")
	}
	expectedUpstream := pushRemote + "/" + branch
	if upstream != expectedUpstream {
		_ = restore()
		return clierror.New(clierror.Conflict, "review branch tracks %q, expected %q", upstream, expectedUpstream)
	}
	checkpoint := localTip
	for _, commit := range selected {
		if err := s.CherryPickOne(ctx, commit.Hash); err != nil {
			if inProgress, progressErr := s.CherryPickInProgress(ctx); progressErr == nil && inProgress {
				_ = s.Abort(ctx)
			}
			rollbackErr := s.ResetHard(ctx, checkpoint)
			restoreErr := restore()
			if rollbackErr != nil || restoreErr != nil {
				return clierror.Wrap(clierror.Conflict, err, "review add conflicted and automatic rollback was incomplete (reset=%v restore=%v)", rollbackErr, restoreErr)
			}
			return clierror.Wrap(clierror.Conflict, err, "review add conflicted; restored %s to %s", branch, checkpoint)
		}
	}
	newTip, err := s.RevisionHash(ctx, "HEAD")
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "read updated review branch tip")
	}
	if err := s.PushCurrent(ctx, pushRemote, branch, false); err != nil {
		_ = restore()
		return clierror.Wrap(clierror.Failure, err, "push %s did not finish; local review branch was kept at %s, so verify the remote before retrying", branch, newTip)
	}

	sourceSet := make(map[string]struct{}, len(state.SourceCommits)+len(selected))
	for _, hash := range state.SourceCommits {
		sourceSet[strings.ToLower(hash)] = struct{}{}
	}
	for _, commit := range selected {
		hash := strings.ToLower(commit.Hash)
		if _, exists := sourceSet[hash]; !exists {
			state.SourceCommits = append(state.SourceCommits, hash)
			sourceSet[hash] = struct{}{}
		}
	}
	state.PublishedTip = newTip
	state.Status = review.StatusOpen
	state.Stage = reviewstate.StageOpen
	if err := reviewstate.Save(ctx, s, state); err != nil {
		_ = restore()
		return clierror.Wrap(clierror.Failure, err, "review branch was pushed but local review state could not be updated; run kit review status before retrying")
	}
	if err := restore(); err != nil {
		return clierror.Wrap(clierror.Failure, err, "review branch was updated and pushed, but the original checkout could not be restored")
	}
	result := reviewAddResult{Branch: branch, ReviewNumber: state.ReviewNumber, ReviewURL: state.ReviewURL, Added: selected, PublishedTip: newTip}
	if opts.json {
		return writeJSON(a.IO.Out, result)
	}
	r := a.renderer(opts.globalOptions)
	r.Success("Push", fmt.Sprintf("%s/%s · %s", pushRemote, branch, newTip[:10]))
	r.Success("Review", fmt.Sprintf("#%d에 %d개 commit 추가", state.ReviewNumber, len(selected)))
	r.Next("PR merge 후 kit review finish")
	return nil
}

func reviewAddPending(ctx context.Context, s gitservice.Service, c gitservice.WorkflowConfig, state reviewstate.State) ([]gitservice.Commit, error) {
	commits, err := s.Candidates(ctx, c.Base, c.Source, true)
	if err != nil {
		return nil, clierror.Wrap(clierror.Failure, err, "list pending work commits")
	}
	commits, err = s.Applied(ctx, c.Base, commits)
	if err != nil {
		return nil, clierror.Wrap(clierror.Failure, err, "filter applied work commits")
	}
	already := make(map[string]struct{}, len(state.SourceCommits))
	for _, hash := range state.SourceCommits {
		already[strings.ToLower(hash)] = struct{}{}
	}
	pending := make([]gitservice.Commit, 0, len(commits))
	for _, commit := range commits {
		if commit.Applied {
			continue
		}
		if _, exists := already[strings.ToLower(commit.Hash)]; exists {
			continue
		}
		pending = append(pending, commit)
	}
	return pending, nil
}

func mergeReviewSourceCommits(ctx context.Context, s gitservice.Service, c gitservice.WorkflowConfig, state reviewstate.State) (reviewstate.State, error) {
	sourceCandidates, err := s.Candidates(ctx, c.Base, c.Source, true)
	if err != nil {
		return state, err
	}
	source := make(map[string]struct{}, len(sourceCandidates))
	for _, commit := range sourceCandidates {
		source[strings.ToLower(commit.Hash)] = struct{}{}
	}
	seen := make(map[string]struct{}, len(state.SourceCommits))
	for _, hash := range state.SourceCommits {
		seen[strings.ToLower(hash)] = struct{}{}
	}
	reviewCommits, err := s.Candidates(ctx, c.Base, state.Branch, true)
	if err != nil {
		return state, err
	}
	changed := false
	for _, commit := range reviewCommits {
		hash := strings.ToLower(commit.Hash)
		if original, ok, sourceErr := s.CherryPickedFrom(ctx, commit.Hash); sourceErr != nil {
			return state, sourceErr
		} else if ok {
			hash = strings.ToLower(original)
		}
		if _, belongs := source[hash]; !belongs {
			continue
		}
		if _, exists := seen[hash]; exists {
			continue
		}
		state.SourceCommits = append(state.SourceCommits, hash)
		seen[hash] = struct{}{}
		changed = true
	}
	if changed {
		if err := reviewstate.Save(ctx, s, state); err != nil {
			return state, err
		}
		return reviewstate.Load(ctx, s, state.Branch)
	}
	return state, nil
}

func reviewAddSummary(selected []gitservice.Commit) string {
	parts := make([]string, 0, len(selected))
	for _, commit := range selected {
		parts = append(parts, ui.SafeText(commit.Subject))
	}
	return strings.Join(parts, ", ")
}

package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"kit/internal/auth"
	"kit/internal/buildinfo"
	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
	"kit/internal/pickstate"
	"kit/internal/review"
	"kit/internal/reviewstate"
	"kit/internal/selector"
	"kit/internal/ui"
	"kit/internal/update"
)

type IO struct {
	In      io.Reader
	Out     io.Writer
	ErrOut  io.Writer
	InFile  *os.File
	OutFile *os.File
}

type Application struct {
	IO                         IO
	Git                        func(dir string) gitservice.Service
	Select                     func([]selector.Item, string) ([]selector.Item, error)
	Update                     func(context.Context, update.Config) (update.Result, error)
	ReviewClient               func(hosting.Repository) (review.Client, error)
	Auth                       AuthService
	AuthInit                   func() (AuthService, error)
	ReadSecret                 func(prompt string) (string, error)
	Build                      buildinfo.Info
	ExecPath                   string
	statusReviewRefreshTimeout time.Duration
}

func New(input *os.File, output, errOutput *os.File) *Application {
	terminal := selector.Terminal{In: input, Out: output}
	application := &Application{
		IO: IO{In: input, Out: output, ErrOut: errOutput, InFile: input, OutFile: output},
		Git: func(dir string) gitservice.Service {
			return gitservice.Service{Dir: dir}
		},
		Select:   terminal.Select,
		Update:   update.Run,
		AuthInit: func() (AuthService, error) { return auth.NewDefault() },
		Build:    buildinfo.Current(),
	}
	application.ReadSecret = terminalSecretReader(input, errOutput)
	application.ReviewClient = application.newReviewClient
	return application
}

type globalOptions struct {
	cwd     string
	json    bool
	noColor bool
	yes     bool
}

func (a *Application) Run(ctx context.Context, args []string) error {
	if a.IO.In == nil {
		a.IO.In = strings.NewReader("")
	}
	if a.IO.Out == nil {
		a.IO.Out = io.Discard
	}
	if a.IO.ErrOut == nil {
		a.IO.ErrOut = io.Discard
	}
	if a.Git == nil {
		a.Git = func(dir string) gitservice.Service { return gitservice.Service{Dir: dir} }
	}
	if a.Update == nil {
		a.Update = update.Run
	}
	if a.AuthInit == nil {
		a.AuthInit = func() (AuthService, error) { return auth.NewDefault() }
	}
	if a.ReadSecret == nil {
		a.ReadSecret = terminalSecretReader(a.IO.InFile, a.IO.ErrOut)
	}
	if a.ReviewClient == nil {
		a.ReviewClient = a.newReviewClient
	}
	if a.Build.Version == "" {
		a.Build = buildinfo.Current()
	}

	global, command, rest, err := parseRoot(args)
	if err != nil {
		return err
	}
	switch command {
	case "", "help":
		printRootHelp(a.IO.Out)
		return nil
	case "status":
		return a.statusCommand(ctx, global, rest)
	case "sync":
		return a.syncCommand(ctx, global, rest)
	case "review":
		return a.gitReview(ctx, global, rest)
	case "backup":
		return a.backupCommand(ctx, global, rest)
	case "git":
		return a.gitCommand(ctx, global, rest)
	case "self":
		return a.selfCommand(ctx, global, rest)
	case "config":
		return a.configCommand(ctx, global, rest)
	case "auth":
		return a.authCommand(ctx, global, rest)
	case "compare":
		return a.compare(ctx, global, rest)
	case "pick":
		return a.pick(ctx, global, rest)
	case "version":
		return a.version(global, rest)
	case "update":
		return a.update(ctx, global, rest)
	case "doctor":
		return a.doctor(ctx, global, rest)
	default:
		return clierror.New(clierror.Usage, "unknown command %q\nRun 'kit help' for usage.", command)
	}
}

func parseRoot(args []string) (globalOptions, string, []string, error) {
	opts := globalOptions{cwd: "."}
	for len(args) > 0 {
		arg := args[0]
		if arg == "-h" || arg == "--help" {
			return opts, "help", nil, nil
		}
		if !strings.HasPrefix(arg, "-") {
			return opts, arg, args[1:], nil
		}
		var consumed int
		var err error
		consumed, err = parseGlobal(&opts, args)
		if err != nil {
			return opts, "", nil, err
		}
		if consumed == 0 {
			return opts, "", nil, clierror.New(clierror.Usage, "unknown global option %q", arg)
		}
		args = args[consumed:]
	}
	return opts, "", nil, nil
}

func parseGlobal(opts *globalOptions, args []string) (int, error) {
	arg := args[0]
	switch arg {
	case "--json":
		opts.json = true
		return 1, nil
	case "--no-color":
		opts.noColor = true
		return 1, nil
	case "--yes":
		opts.yes = true
		return 1, nil
	case "--cwd":
		if len(args) < 2 || args[1] == "" {
			return 0, clierror.New(clierror.Usage, "--cwd requires a path")
		}
		opts.cwd = args[1]
		return 2, nil
	default:
		if strings.HasPrefix(arg, "--cwd=") {
			opts.cwd = strings.TrimPrefix(arg, "--cwd=")
			if opts.cwd == "" {
				return 0, clierror.New(clierror.Usage, "--cwd requires a path")
			}
			return 1, nil
		}
	}
	return 0, nil
}

type compareOptions struct {
	globalOptions
	source    string
	base      string
	limit     int
	sourceSet bool
	baseSet   bool
	fetch     bool
}

func parseCompare(global globalOptions, args []string) (compareOptions, bool, error) {
	opts := compareOptions{globalOptions: global, source: "work", base: "develop"}
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
		case arg == "--base":
			if len(args) < 2 {
				return opts, false, clierror.New(clierror.Usage, "--base requires a branch")
			}
			opts.base, opts.baseSet, args = args[1], true, args[2:]
		case strings.HasPrefix(arg, "--base="):
			opts.base, opts.baseSet, args = strings.TrimPrefix(arg, "--base="), true, args[1:]
		case arg == "--fetch":
			opts.fetch, args = true, args[1:]
		case arg == "--limit":
			if len(args) < 2 {
				return opts, false, clierror.New(clierror.Usage, "--limit requires a positive integer")
			}
			value, err := positiveInt(args[1])
			if err != nil {
				return opts, false, err
			}
			opts.limit, args = value, args[2:]
		case strings.HasPrefix(arg, "--limit="):
			value, err := positiveInt(strings.TrimPrefix(arg, "--limit="))
			if err != nil {
				return opts, false, err
			}
			opts.limit, args = value, args[1:]
		case strings.HasPrefix(arg, "-"):
			return opts, false, clierror.New(clierror.Usage, "unknown compare option %q", arg)
		default:
			positionals, args = append(positionals, arg), args[1:]
		}
	}
	if len(positionals) > 1 {
		return opts, false, clierror.New(clierror.Usage, "compare accepts at most one source branch")
	}
	if len(positionals) == 1 {
		opts.source = positionals[0]
		opts.sourceSet = true
	}
	if opts.base == "" || opts.source == "" {
		return opts, false, clierror.New(clierror.Usage, "source and base branch must not be empty")
	}
	return opts, false, nil
}

func positiveInt(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, clierror.New(clierror.Usage, "--limit requires a positive integer")
	}
	return n, nil
}

type compareResult struct {
	Source  string              `json:"source"`
	Base    string              `json:"base"`
	Commits []gitservice.Commit `json:"commits"`
	Applied int                 `json:"applied"`
	Pending int                 `json:"pending"`
}

func (a *Application) compare(ctx context.Context, global globalOptions, args []string) error {
	opts, help, err := parseCompare(global, args)
	if err != nil {
		return err
	}
	if help {
		printCompareHelp(a.IO.Out)
		return nil
	}
	service, err := a.validatedGit(ctx, opts.cwd)
	if err != nil {
		return err
	}
	config := service.WorkflowConfig(ctx)
	if !opts.baseSet {
		opts.base = config.Base
	}
	if !opts.sourceSet {
		opts.source = config.Source
	}
	if opts.fetch {
		if err := service.Fetch(ctx, config.Remote); err != nil {
			return clierror.Wrap(clierror.Failure, err, "fetch %s", config.Remote)
		}
	}
	if err := verifyRevisions(ctx, service, opts.base, opts.source); err != nil {
		return err
	}
	commits, err := service.Candidates(ctx, opts.base, opts.source, false)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "list commits from %s", opts.source)
	}
	if opts.limit > 0 && len(commits) > opts.limit {
		commits = commits[:opts.limit]
	}
	commits, err = service.Applied(ctx, opts.base, commits)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "compare patches against %s", opts.base)
	}
	result := compareResult{Source: opts.source, Base: opts.base, Commits: commits}
	for _, commit := range commits {
		if commit.Applied {
			result.Applied++
		} else {
			result.Pending++
		}
	}
	if opts.json {
		return writeJSON(a.IO.Out, result)
	}
	a.printCompare(result, !opts.noColor && isTerminal(a.IO.Out))
	return nil
}

func (a *Application) validatedGit(ctx context.Context, dir string, revisions ...string) (gitservice.Service, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return gitservice.Service{}, clierror.Wrap(clierror.Failure, err, "resolve --cwd")
	}
	service := a.Git(absolute)
	if err := service.ValidateDependency(ctx); err != nil {
		return service, clierror.Wrap(clierror.Failure, err, "Git dependency check failed")
	}
	if err := service.ValidateRepository(ctx); err != nil {
		return service, clierror.Wrap(clierror.Failure, err, "%s is not a Git repository", absolute)
	}
	for _, revision := range revisions {
		if err := service.VerifyRevision(ctx, revision); err != nil {
			return service, clierror.Wrap(clierror.Failure, err, "branch or revision %q was not found", revision)
		}
	}
	return service, nil
}

func verifyRevisions(ctx context.Context, service gitservice.Service, revisions ...string) error {
	for _, revision := range revisions {
		if err := service.VerifyRevision(ctx, revision); err != nil {
			return clierror.Wrap(clierror.Failure, err, "branch or revision %q was not found", revision)
		}
	}
	return nil
}

func (a *Application) printCompare(result compareResult, color bool) {
	bold, green, yellow, cyan, gray, reset := "", "", "", "", "", ""
	if color {
		bold, green, yellow, cyan, gray, reset = "\x1b[1m", "\x1b[32m", "\x1b[33m", "\x1b[36m", "\x1b[90m", "\x1b[0m"
	}
	renderer := ui.Renderer{Writer: a.IO.Out, Color: color}
	renderer.Command("compare")
	renderer.Field("Flow", fmt.Sprintf("%s → %s", result.Source, result.Base))
	fmt.Fprintln(a.IO.Out)
	fmt.Fprintf(a.IO.Out, "%s%-4s %-10s %-16s %s%s\n", bold, "STAT", "HASH", "DATE", "MESSAGE", reset)
	for _, commit := range result.Commits {
		status, statusColor := "●", yellow
		if commit.Applied {
			status, statusColor = "✓", green
		}
		fmt.Fprintf(a.IO.Out, "%s%s%s    %s%-10s%s %s%-16s%s %s\n", statusColor, status, reset, cyan, commit.ShortHash, reset, gray, commit.Date, reset, ui.SafeText(commit.Subject))
	}
	fmt.Fprintln(a.IO.Out)
	renderer.Success("Applied", fmt.Sprintf("%d개", result.Applied))
	if result.Pending > 0 {
		renderer.Pending("Pending", fmt.Sprintf("%d개", result.Pending))
	} else {
		renderer.Success("Pending", "없음")
	}
}

type pickOptions struct {
	globalOptions
	target     string
	source     string
	base       string
	sourceSet  bool
	baseSet    bool
	fetch      bool
	allowStale bool
	submit     bool
	localOnly  bool
	wait       bool
	action     string
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
		case arg == "--submit":
			opts.submit, args = true, args[1:]
		case arg == "--local":
			opts.localOnly, args = true, args[1:]
		case arg == "--wait":
			opts.submit, opts.wait, args = true, true, args[1:]
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
	if opts.action != "" && (opts.submit || opts.localOnly || opts.wait) {
		return opts, false, clierror.New(clierror.Usage, "pick resume actions use the submit settings saved by the original pick")
	}
	if opts.localOnly && (opts.submit || opts.wait) {
		return opts, false, clierror.New(clierror.Usage, "--local cannot be combined with --submit or --wait")
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
	if opts.submit {
		ctx = withInsecureHTTPWarningOnce(ctx)
		repository, err := reviewRepository(ctx, service, config)
		if err != nil {
			return err
		}
		warnInsecureHTTP(ctx, a.IO.ErrOut, repository)
		if _, err := a.ReviewClient(repository); err != nil {
			return clierror.Wrap(clierror.Failure, err, "initialize review provider before pick")
		}
	}
	if opts.fetch || opts.submit {
		if err := service.Fetch(ctx, config.Remote); err != nil {
			return clierror.Wrap(clierror.Failure, err, "fetch %s", config.Remote)
		}
	}
	if err := verifyRevisions(ctx, service, opts.base, opts.source); err != nil {
		return err
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
	selectedItems, err := a.Select(items, fmt.Sprintf("%s → %s (%s에서 새 브랜치)", opts.source, opts.target, opts.base))
	if errors.Is(err, selector.ErrCanceled) {
		fmt.Fprintln(a.IO.Out, "취소되었습니다.")
		return nil
	}
	if errors.Is(err, selector.ErrInterrupted) {
		return clierror.New(clierror.Interrupt, "interrupted")
	}
	if errors.Is(err, selector.ErrNotTTY) {
		return clierror.New(clierror.Failure, "kit pick requires an interactive TTY; non-interactive commit selection is not supported")
	}
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "open commit selector")
	}
	if len(selectedItems) == 0 {
		fmt.Fprintln(a.IO.Out, "선택한 커밋이 없어 취소되었습니다.")
		return nil
	}
	selected := make([]gitservice.Commit, 0, len(selectedItems))
	for _, item := range selectedItems {
		selected = append(selected, byHash[item.ID])
	}
	a.printPickSummary(opts, selected)
	reader := bufio.NewReader(a.IO.In)
	if !opts.yes {
		prompt := "선택한 커밋으로 로컬 브랜치를 만드시겠습니까? [y/N] "
		if opts.submit {
			prompt = "선택한 커밋으로 브랜치를 만들고 push와 Gitea PR을 생성하시겠습니까? [y/N] "
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
	originalHash, originalBranch, err := service.Head(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "record current checkout")
	}
	state := pickstate.State{
		OriginalHash: originalHash, OriginalBranch: originalBranch, TargetBranch: opts.target,
		BaseBranch: opts.base, SourceBranch: opts.source,
		SubmitAfterPick: opts.submit, WaitAfterSubmit: opts.wait,
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
	return a.runPick(ctx, opts.globalOptions, service, state, reader)
}

func (a *Application) runPick(ctx context.Context, global globalOptions, service gitservice.Service, state pickstate.State, reader *bufio.Reader) error {
	for {
		if state.Next >= len(state.Commits) {
			tip, err := service.RevisionHash(ctx, state.TargetBranch)
			if err != nil {
				return clierror.Wrap(clierror.Failure, err, "read completed review branch")
			}
			reviewState := reviewstate.State{
				Stage: reviewstate.StagePicked, Branch: state.TargetBranch,
				SourceBranch: state.SourceBranch, TargetBranch: state.BaseBranch,
				SourceCommits: append([]string(nil), state.Commits...), PickedTip: tip,
			}
			if err := reviewstate.Save(ctx, service, reviewState); err != nil {
				return clierror.Wrap(clierror.Failure, err, "save completed review state")
			}
			if err := pickstate.Remove(ctx, service); err != nil {
				return clierror.Wrap(clierror.Failure, err, "remove completed pick state")
			}
			if state.SubmitAfterPick {
				pushAttempted := false
				err := a.reviewSubmit(ctx, global, reviewSubmitOptions{
					wait: state.WaitAfterSubmit, removeSourceBranch: true, confirmed: true,
					pushAttempted: &pushAttempted,
				}, &service)
				if err == nil {
					return nil
				}
				if pushAttempted {
					return clierror.Wrap(clierror.Code(err), err, "review submit did not finish after push started; %s and its review state were kept for retry", state.TargetBranch)
				}
				if rollbackErr := rollbackPickBeforePush(service, state); rollbackErr != nil {
					return clierror.Wrap(clierror.Code(err), err, "review submit failed before push; automatic rollback was incomplete: %v", rollbackErr)
				}
				return clierror.Wrap(clierror.Code(err), err, "review submit failed before push; restored %s and removed %s", state.OriginalBranch, state.TargetBranch)
			}
			renderer := a.renderer(global)
			renderer.Command("pick")
			renderer.Success("Branch", fmt.Sprintf("%s · %d개 커밋", state.TargetBranch, len(state.Commits)))
			renderer.Next("kit review submit")
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
	if err := reviewstate.Delete(rollbackCtx, service, state.TargetBranch); err != nil {
		return fmt.Errorf("target branch %s was removed, but picked review state cleanup failed: %w", state.TargetBranch, err)
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
		return a.runPick(ctx, global, service, state, bufio.NewReader(a.IO.In))
	}
	if err := a.advancePick(ctx, service, &state, action); err != nil {
		return err
	}
	return a.runPick(ctx, global, service, state, bufio.NewReader(a.IO.In))
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
	if err := pickstate.Remove(ctx, service); err != nil {
		return clierror.Wrap(clierror.Failure, err, "remove pick state")
	}
	return clierror.New(clierror.Failure, "cherry-pick aborted; restored the original checkout and removed %q", state.TargetBranch)
}

func (a *Application) printPickSummary(opts pickOptions, selected []gitservice.Commit) {
	renderer := a.renderer(opts.globalOptions)
	renderer.Command("pick plan")
	renderer.Section("브랜치")
	renderer.Field("New", opts.target)
	renderer.Field("Base", opts.base)
	renderer.Field("Work", opts.source)
	if opts.submit {
		renderer.Field("Action", "브랜치 생성 · push · Gitea PR 생성")
	} else {
		renderer.Field("Action", "로컬 브랜치 생성")
	}
	renderer.Section(fmt.Sprintf("선택한 커밋 · %d개", len(selected)))
	for _, commit := range selected {
		renderer.Pending(commit.ShortHash, commit.Subject)
	}
	fmt.Fprintln(a.IO.Out)
}

func (a *Application) version(global globalOptions, args []string) error {
	for len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Fprintln(a.IO.Out, "Usage: kit version [--json]")
			return nil
		}
		consumed, err := parseGlobal(&global, args)
		if err != nil {
			return err
		}
		if consumed == 0 {
			return clierror.New(clierror.Usage, "unknown version option %q", args[0])
		}
		args = args[consumed:]
	}
	if global.json {
		return writeJSON(a.IO.Out, a.Build)
	}
	fmt.Fprintf(a.IO.Out, "kit %s\ncommit: %s\nbuilt: %s\ntarget: %s\n", a.Build.Version, a.Build.Commit, a.Build.BuildDate, a.Build.Target)
	return nil
}

func (a *Application) update(ctx context.Context, global globalOptions, args []string) error {
	for len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Fprintln(a.IO.Out, "Usage: kit update [--json]")
			return nil
		}
		consumed, err := parseGlobal(&global, args)
		if err != nil {
			return err
		}
		if consumed == 0 {
			return clierror.New(clierror.Usage, "unknown update option %q", args[0])
		}
		args = args[consumed:]
	}
	result, err := a.Update(ctx, update.Config{Current: a.Build, Executable: a.ExecPath})
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "update failed")
	}
	if global.json {
		return writeJSON(a.IO.Out, result)
	}
	if result.Updated {
		fmt.Fprintf(a.IO.Out, "kit %s → %s 업데이트 완료\n", result.Current, result.Latest)
	} else {
		fmt.Fprintf(a.IO.Out, "이미 최신 버전입니다: %s\n", result.Current)
	}
	return nil
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return clierror.Wrap(clierror.Failure, err, "write JSON output")
	}
	return nil
}

func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func (a *Application) renderer(global globalOptions) ui.Renderer {
	color := !global.noColor && os.Getenv("NO_COLOR") == "" && isTerminal(a.IO.Out)
	return ui.Renderer{Writer: a.IO.Out, Color: color}
}

func printRootHelp(w io.Writer) {
	fmt.Fprint(w, `kit — repeatable developer workflow tools

Usage:
  kit [global options] <command> [arguments]

Commands:
  status    Show the work queue, base synchronization, and tracked reviews
  pick      Select work commits, push a branch, and create a Gitea PR
  sync      Finish a merged review or synchronize develop and work
  review    Advanced review inspection and recovery commands
  backup    List, create, restore, and clean work backups

Setup and maintenance:
  auth      Gitea credential storage commands
  config    Repository-local workflow settings
  doctor    Check dependencies and repository configuration
  update    Update the installed kit binary
  version   Print build information

Advanced compatibility:
  compare   Show commit-level work/base comparison
  git       Legacy namespace; existing 'kit git ...' commands remain supported
  self      Legacy namespace for version, update, and doctor

Global options:
  --cwd <path>   Run Git commands in another directory
  --json         Print machine-readable output where supported
  --no-color     Disable ANSI colors
  --yes          Skip mutation confirmation
`)
}

func printCompareHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: kit compare [source] [options]

Options:
  --base <branch>  Base branch (default: develop)
  --limit <n>      Show only the newest n source commits
  --fetch          Fetch the configured remote before comparing
  --cwd <path>     Git repository directory
  --json           Print JSON
  --no-color       Disable ANSI colors
`)
}

func printPickHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: kit pick <new-branch> [options]

Options:
  --from <branch>  Source branch; custom values require --local (default: work)
  --base <branch>  Base branch; custom values require --local (default: develop)
  --fetch          Fetch the configured remote before selection
  --allow-stale    Allow an outdated source branch (recovery only)
  --local          Create the local branch without push or PR creation
  --wait           Create the PR, then wait for its result
  --submit         Compatibility option; submission is now the default
  --continue       Continue a paused kit pick after staging resolutions
  --skip           Skip the current commit in a paused kit pick
  --abort          Abort a paused kit pick and restore the original checkout
  --cwd <path>     Git repository directory
  --yes            Skip confirmation after commit selection
  --no-color       Disable ANSI colors outside the selector
`)
}

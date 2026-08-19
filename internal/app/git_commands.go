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
}

func (a *Application) gitStatus(ctx context.Context, global globalOptions, args []string) error {
	fetch := false
	for len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Fprintln(a.IO.Out, "Usage: kit git status [--fetch] [--json] [--cwd <path>]")
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
	state := "동기화됨"
	if !result.SourceSynced {
		state = "동기화 필요"
	}
	renderer := a.renderer(global)
	renderer.Command("git status")
	renderer.Field("Provider", result.Provider)
	renderer.Field("Remote", config.Remote)
	renderer.Field("Base", config.Base)
	renderer.Field("Work", config.Source)
	renderer.Field("Current", branch)
	renderer.Field("Tree", cleanLabel(clean))
	renderer.Field("Operation", result.Operation)
	renderer.Field("Queue", fmt.Sprintf("%s · 대기 %d", state, result.Pending))
	if result.RemoteObserved {
		renderer.Field("Base remote", fmt.Sprintf("ahead %d · behind %d", result.BaseAhead, result.BaseBehind))
	}
	if !result.SourceSynced {
		renderer.Warning("Work", "현재 base를 포함하지 않습니다.")
		renderer.Next("kit git sync")
	}
	return nil
}

func cleanLabel(clean bool) string {
	if clean {
		return "clean"
	}
	return "dirty"
}

type syncOptions struct {
	dryRun   bool
	baseOnly bool
}

func (a *Application) gitSync(ctx context.Context, global globalOptions, args []string) error {
	opts := syncOptions{}
	for len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Fprintln(a.IO.Out, "Usage: kit git sync [--dry-run] [--base-only] [--yes] [--json] [--cwd <path>]")
			return nil
		}
		if consumed, err := parseGlobal(&global, args); err != nil {
			return err
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
			return clierror.New(clierror.Usage, "unknown git sync option %q", args[0])
		}
	}
	if global.json && !global.yes && !opts.dryRun {
		return clierror.New(clierror.Usage, "git sync --json requires --yes or --dry-run")
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

func syncCommandError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		if err != nil {
			return clierror.Wrap(clierror.Interrupt, err, "sync interrupted after restoring repository state")
		}
		return clierror.New(clierror.Interrupt, "sync interrupted after repository state was finalized; run 'kit git status' to verify")
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
	renderer.Command("git sync")
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
	if result.Pushed && (result.Provider == "gitlab" || result.Provider == "forgejo") {
		renderer.Next("kit git review submit")
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
		prefix := "kit/backup/" + strings.ReplaceAll(config.Source, "/", "-") + "-"
		backups, err := service.ListRefs(ctx, prefix)
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "list work backups")
		}
		if global.json {
			return writeJSON(a.IO.Out, backups)
		}
		if len(backups) == 0 {
			fmt.Fprintln(a.IO.Out, "work backup이 없습니다.")
			return nil
		}
		for _, backup := range backups {
			fmt.Fprintln(a.IO.Out, backup)
		}
		return nil
	case "backup":
		if len(rest) != 0 {
			return clierror.New(clierror.Usage, "git work backup accepts no arguments")
		}
		return a.createWorkBackup(ctx, service, config)
	case "restore":
		if len(rest) != 1 {
			return clierror.New(clierror.Usage, "git work restore requires a backup branch")
		}
		return a.restoreWorkBackup(ctx, service, config, rest[0], global.yes)
	default:
		return clierror.New(clierror.Usage, "unknown git work command %q", command)
	}
}

func (a *Application) createWorkBackup(ctx context.Context, service gitservice.Service, config gitservice.WorkflowConfig) error {
	hash, err := service.RevisionHash(ctx, config.Source)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "read %s", config.Source)
	}
	short := hash
	if len(short) > 8 {
		short = short[:8]
	}
	name := fmt.Sprintf("kit/backup/%s-manual-%s", strings.ReplaceAll(config.Source, "/", "-"), short)
	if exists, _ := service.LocalBranchExists(ctx, name); exists {
		return clierror.New(clierror.Conflict, "backup %s already exists", name)
	}
	if err := service.CreateBranchAt(ctx, name, hash); err != nil {
		return clierror.Wrap(clierror.Failure, err, "create work backup")
	}
	fmt.Fprintln(a.IO.Out, name)
	return nil
}

func (a *Application) restoreWorkBackup(ctx context.Context, service gitservice.Service, config gitservice.WorkflowConfig, backup string, yes bool) error {
	prefix := "kit/backup/" + strings.ReplaceAll(config.Source, "/", "-") + "-"
	if !strings.HasPrefix(backup, prefix) {
		return clierror.New(clierror.Usage, "backup must start with %q", prefix)
	}
	if err := service.VerifyRevision(ctx, backup); err != nil {
		return clierror.Wrap(clierror.Failure, err, "backup %q was not found", backup)
	}
	clean, err := service.IsClean(ctx)
	if err != nil || !clean {
		return clierror.New(clierror.Failure, "working tree must be clean before restore")
	}
	if !yes {
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
	safetyBackup := fmt.Sprintf("kit/backup/%s-before-restore-%s-%s", strings.ReplaceAll(config.Source, "/", "-"), time.Now().UTC().Format("20060102-150405"), short)
	if err := service.CreateBranchAt(ctx, safetyBackup, currentSourceHash); err != nil {
		return clierror.Wrap(clierror.Failure, err, "create pre-restore safety backup")
	}
	if originalBranch == config.Source {
		if err := service.SwitchDetach(ctx, originalHash); err != nil {
			return clierror.Wrap(clierror.Failure, err, "detach before restore")
		}
	}
	if err := service.ForceBranch(ctx, config.Source, backupHash); err != nil {
		if originalBranch == config.Source {
			_ = service.Switch(ctx, config.Source)
		}
		return clierror.Wrap(clierror.Failure, err, "restore %s", config.Source)
	}
	if originalBranch == config.Source {
		if err := service.Switch(ctx, config.Source); err != nil {
			return clierror.Wrap(clierror.Failure, err, "switch to restored %s", config.Source)
		}
	}
	fmt.Fprintf(a.IO.Out, "%s → %s\nsafety backup: %s\n", config.Source, backup, safetyBackup)
	return nil
}

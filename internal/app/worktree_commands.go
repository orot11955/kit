package app

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
)

type worktreeAddOptions struct {
	branch string
	path   string
	base   string
	create bool
}

func (a *Application) worktreeCommand(ctx context.Context, global globalOptions, args []string) error {
	var err error
	global, args, err = parseLeadingGlobals(global, args)
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printWorktreeHelp(a.IO.Out)
		return nil
	}
	command, rest := args[0], args[1:]
	switch command {
	case "list":
		g, remaining, err := parseAllGlobals(global, rest)
		if err != nil {
			return err
		}
		if len(remaining) != 0 {
			return clierror.New(clierror.Usage, "worktree list accepts no arguments")
		}
		return a.worktreeList(ctx, g)
	case "add":
		return a.worktreeAdd(ctx, global, rest)
	case "remove":
		return a.worktreeRemove(ctx, global, rest)
	case "prune":
		g, remaining, err := parseAllGlobals(global, rest)
		if err != nil {
			return err
		}
		if len(remaining) != 0 {
			return clierror.New(clierror.Usage, "worktree prune accepts no arguments")
		}
		service, err := a.validatedGit(ctx, g.cwd)
		if err != nil {
			return err
		}
		if err := service.PruneWorktrees(ctx); err != nil {
			return clierror.Wrap(clierror.Failure, err, "prune worktrees")
		}
		if g.json {
			return writeJSON(a.IO.Out, map[string]any{"pruned": true})
		}
		a.renderer(g).Success("Worktree", "prune 완료")
		return nil
	default:
		return clierror.New(clierror.Usage, "unknown worktree command %q", command)
	}
}

func (a *Application) worktreeList(ctx context.Context, global globalOptions) error {
	service, err := a.validatedGit(ctx, global.cwd)
	if err != nil {
		return err
	}
	items, err := service.Worktrees(ctx)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "list worktrees")
	}
	if global.json {
		return writeJSON(a.IO.Out, items)
	}
	r := a.renderer(global)
	r.Command("worktree list")
	if len(items) == 0 {
		r.Field("Worktree", "없음")
		return nil
	}
	for _, item := range items {
		branch := item.Branch
		if branch == "" {
			branch = "(detached)"
		}
		r.Field("Worktree", fmt.Sprintf("%s · %s · %s", item.Path, branch, shortHash(item.Head)))
	}
	return nil
}

func (a *Application) worktreeAdd(ctx context.Context, global globalOptions, args []string) error {
	opts := worktreeAddOptions{}
	positionals := []string{}
	for len(args) > 0 {
		if consumed, err := parseGlobal(&global, args); err != nil {
			return err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		switch {
		case args[0] == "--create":
			opts.create = true
			args = args[1:]
		case args[0] == "--base":
			if len(args) < 2 || args[1] == "" {
				return clierror.New(clierror.Usage, "--base requires a branch")
			}
			opts.base, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--base="):
			opts.base, args = strings.TrimPrefix(args[0], "--base="), args[1:]
		case strings.HasPrefix(args[0], "-"):
			return clierror.New(clierror.Usage, "unknown worktree add option %q", args[0])
		default:
			positionals, args = append(positionals, args[0]), args[1:]
		}
	}
	if len(positionals) < 1 || len(positionals) > 2 {
		return clierror.New(clierror.Usage, "worktree add requires <branch> and optional [path]")
	}
	opts.branch = positionals[0]
	if len(positionals) == 2 {
		opts.path = positionals[1]
	}

	service, err := a.validatedGit(ctx, global.cwd)
	if err != nil {
		return err
	}
	config := service.WorkflowConfig(ctx)
	if opts.base == "" {
		opts.base = config.Base
	}
	if err := service.ValidateBranchName(ctx, opts.branch); err != nil {
		return clierror.Wrap(clierror.Usage, err, "invalid worktree branch %q", opts.branch)
	}
	exists, err := service.LocalBranchExists(ctx, opts.branch)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "check branch %q", opts.branch)
	}
	if opts.create && exists {
		return clierror.New(clierror.Conflict, "branch %q already exists; omit --create to attach it", opts.branch)
	}
	if !opts.create && !exists {
		return clierror.New(clierror.Conflict, "branch %q does not exist; use --create to create it from %s", opts.branch, opts.base)
	}
	if opts.path == "" {
		root, err := service.TopLevel(ctx)
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "resolve repository root")
		}
		name := filepath.Base(root) + "-" + worktreePathName(opts.branch)
		opts.path = filepath.Join(filepath.Dir(root), name)
	}
	path, err := filepath.Abs(opts.path)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "resolve worktree path")
	}
	path = filepath.Clean(path)
	if opts.create {
		if err := service.VerifyRevision(ctx, opts.base); err != nil {
			return clierror.Wrap(clierror.Failure, err, "base branch %q was not found", opts.base)
		}
		err = service.AddWorktreeBranch(ctx, path, opts.branch, opts.base)
	} else {
		err = service.AddWorktree(ctx, path, opts.branch)
	}
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "add worktree %s", path)
	}
	item := map[string]any{"path": path, "branch": opts.branch, "created_branch": opts.create}
	if opts.create {
		item["base"] = opts.base
	}
	if global.json {
		return writeJSON(a.IO.Out, item)
	}
	r := a.renderer(global)
	r.Command("worktree add")
	r.Success("Path", path)
	r.Success("Branch", opts.branch)
	return nil
}

func (a *Application) worktreeRemove(ctx context.Context, global globalOptions, args []string) error {
	force := false
	positionals := []string{}
	for len(args) > 0 {
		if consumed, err := parseGlobal(&global, args); err != nil {
			return err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		switch args[0] {
		case "--force":
			force, args = true, args[1:]
		default:
			if strings.HasPrefix(args[0], "-") {
				return clierror.New(clierror.Usage, "unknown worktree remove option %q", args[0])
			}
			positionals, args = append(positionals, args[0]), args[1:]
		}
	}
	if len(positionals) != 1 {
		return clierror.New(clierror.Usage, "worktree remove requires exactly one path")
	}
	path, err := filepath.Abs(positionals[0])
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "resolve worktree path")
	}
	service, err := a.validatedGit(ctx, global.cwd)
	if err != nil {
		return err
	}
	if global.json && !global.yes {
		return clierror.New(clierror.Usage, "worktree remove --json requires --yes")
	}
	if !global.yes {
		ok, err := confirm(a.IO.In, a.IO.Out, fmt.Sprintf("worktree %s를 제거하시겠습니까? branch는 삭제하지 않습니다. [y/N] ", filepath.Clean(path)))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}
	if err := service.RemoveWorktree(ctx, filepath.Clean(path), force); err != nil {
		return clierror.Wrap(clierror.Failure, err, "remove worktree")
	}
	if global.json {
		return writeJSON(a.IO.Out, map[string]any{"removed": true, "path": filepath.Clean(path), "branch_deleted": false})
	}
	r := a.renderer(global)
	r.Success("Worktree", "제거 완료")
	r.Field("Branch", "보존")
	return nil
}

func worktreePathName(branch string) string {
	value := strings.NewReplacer("/", "-", "\\", "-", " ", "-").Replace(branch)
	value = strings.Trim(value, ".-")
	if value == "" {
		return "worktree"
	}
	return value
}

func shortHash(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

func printWorktreeHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: kit worktree <command>

Commands:
  list                          List linked worktrees
  add <branch> [path]           Attach an existing local branch
  add <branch> [path] --create  Create branch from base and attach it
  remove <path>                 Remove a linked worktree; keep the branch
  prune                         Prune stale worktree administrative data

Options:
  --base <branch>  Base for --create (default: configured develop)
  --force          Force worktree removal
  --cwd <path>     Git repository directory
  --json           Print machine-readable output where supported
  --yes            Skip removal confirmation
`)
}

var _ = gitservice.Worktree{}

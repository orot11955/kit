package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	gitservice "kit/internal/git"
)

// RunCLI keeps the established Application.Run contract intact while routing
// newer developer-only top-level commands out of the already-large app.go
// orchestration file. Commands not handled here are delegated unchanged.
func (a *Application) RunCLI(ctx context.Context, args []string) error {
	extendedCtx, stripped, verbose := stripVerboseFlag(ctx, args)
	global, command, rest, err := parseRoot(stripped)
	if err != nil {
		return a.Run(ctx, args)
	}
	if command == "" || command == "help" {
		if err := a.Run(ctx, args); err != nil {
			return err
		}
		fmt.Fprint(a.IO.Out, `
Additional developer tools:
  worktree      Manage linked Git worktrees
  branch-clean  Dry-run and clean safe Kit-created local review branches
`)
		return nil
	}
	if command != "worktree" && command != "branch-clean" {
		return a.Run(ctx, args)
	}
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
	if verbose {
		restoreGit := a.enableVerboseGit()
		defer restoreGit()
	}
	switch command {
	case "worktree":
		return a.worktreeCommand(extendedCtx, global, rest)
	case "branch-clean":
		return a.branchCleanCommand(extendedCtx, global, rest)
	default:
		return a.Run(ctx, args)
	}
}

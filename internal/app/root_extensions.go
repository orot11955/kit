package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	"kit/internal/auth"
	gitservice "kit/internal/git"
)

// RunCLI keeps the established Application.Run contract intact while routing
// newer top-level/option extensions out of the already-large app.go
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

Additional diagnostics/options:
  doctor --recovery       Inspect interrupted operations and recovery refs
  review list --refresh   Refresh saved active review states from the provider
  review finish --json    Finish a merged review non-interactively with --yes
`)
		return nil
	}

	extended := command == "worktree" || command == "branch-clean" ||
		(command == "review" && isExtendedReviewCommand(global, rest)) ||
		(command == "doctor" && isRecoveryDoctorCommand(rest))
	if !extended {
		return a.Run(ctx, args)
	}
	prepareExtensionIOAndGit(a)
	if verbose {
		restoreGit := a.enableVerboseGit()
		defer restoreGit()
	}

	switch command {
	case "worktree":
		return a.worktreeCommand(extendedCtx, global, rest)
	case "branch-clean":
		return a.branchCleanCommand(extendedCtx, global, rest)
	case "doctor":
		return a.doctorRecoveryCommand(extendedCtx, global, rest)
	case "review":
		restore := a.prepareReviewExtension(global, rest)
		defer restore()
		return a.reviewExtensionCommand(extendedCtx, global, rest)
	default:
		return a.Run(ctx, args)
	}
}

func prepareExtensionIOAndGit(a *Application) {
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
}

func (a *Application) prepareReviewExtension(global globalOptions, rest []string) func() {
	if a.AuthInit == nil {
		a.AuthInit = func() (AuthService, error) { return auth.NewDefault() }
	}
	if a.ReviewClient == nil {
		a.ReviewClient = a.newReviewClient
	}
	previous := a.allowKeychainUnlock
	a.allowKeychainUnlock = !global.json && !argumentsContainJSON(rest)
	return func() { a.allowKeychainUnlock = previous }
}

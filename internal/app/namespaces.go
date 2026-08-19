package app

import (
	"context"
	"fmt"

	"kit/internal/clierror"
)

func (a *Application) gitCommand(ctx context.Context, global globalOptions, args []string) error {
	var err error
	global, args, err = parseLeadingGlobals(global, args)
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printGitHelp(a.IO.Out)
		return nil
	}
	switch args[0] {
	case "status":
		return a.gitStatus(ctx, global, args[1:])
	case "compare":
		return a.compare(ctx, global, args[1:])
	case "pick":
		return a.pick(ctx, global, args[1:])
	case "sync":
		return a.gitSync(ctx, global, args[1:])
	case "publish":
		return a.gitPublish(ctx, global, args[1:])
	case "review":
		return a.gitReview(ctx, global, args[1:])
	case "work":
		return a.gitWork(ctx, global, args[1:])
	default:
		return clierror.New(clierror.Usage, "unknown git command %q\nRun 'kit git help' for usage.", args[0])
	}
}

func (a *Application) selfCommand(ctx context.Context, global globalOptions, args []string) error {
	var err error
	global, args, err = parseLeadingGlobals(global, args)
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(a.IO.Out, `Usage: kit self <command>

Commands:
  version   Print build version information
  update    Safely update an installer-managed kit binary
  doctor    Check kit's runtime dependencies and repository workflow
`)
		return nil
	}
	switch args[0] {
	case "version":
		return a.version(global, args[1:])
	case "update":
		return a.update(ctx, global, args[1:])
	case "doctor":
		return a.doctor(ctx, global, args[1:])
	default:
		return clierror.New(clierror.Usage, "unknown self command %q", args[0])
	}
}

func parseLeadingGlobals(global globalOptions, args []string) (globalOptions, []string, error) {
	for len(args) > 0 && len(args[0]) > 0 && args[0][0] == '-' {
		consumed, err := parseGlobal(&global, args)
		if err != nil {
			return global, args, err
		}
		if consumed == 0 {
			break
		}
		args = args[consumed:]
	}
	return global, args, nil
}

func printGitHelp(w interface{ Write([]byte) (int, error) }) {
	fmt.Fprint(w, `Usage: kit git <command>

Commands:
  status    Show the configured branch workflow and synchronization state
  compare   Compare source commits with a base branch
  pick      Select pending commits and apply them to a new review branch
  sync      Fast-forward the base and rebuild work from pending commits
  publish   Push the current review branch and print its review URL
  review    Submit, track, finish, and list GitLab MRs or Forgejo PRs
  work      Manage work backups created by sync
`)
}

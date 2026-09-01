package app

import (
	"fmt"
	"io"
)

func printRootHelp(w io.Writer) {
	fmt.Fprint(w, `kit — repeatable developer workflow tools

Usage:
  kit [global options] <command> [arguments]

Daily workflow:
  compare   Show pending work commits and already-applied work
  pick      Select commits, push a branch, and create a PR

Advanced and recovery:
  status    Show detailed repository, queue, and tracked-review state
  sync      Update develop and rebuild work from first-parent pending commits
  review    Advanced review inspection and recovery commands
  backup    List, create, restore, and clean work backups

Setup and maintenance:
  auth      Gitea credential storage commands
  config    Repository-local workflow settings
  doctor    Check dependencies and repository configuration
  update    Update the installed kit binary
  version   Print build information

Compatibility:
  git       Legacy namespace; existing 'kit git ...' commands remain supported
  self      Legacy namespace for version, update, and doctor

Global options:
  --cwd <path>   Run Git commands in another directory
  --json         Print machine-readable output where supported
  --no-color     Disable ANSI colors
  --yes          Skip mutation confirmation
  --verbose      Print sanitized Git and provider diagnostics to stderr
`)
}

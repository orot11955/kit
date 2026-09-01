package app

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/ui"
)

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
	Source         string              `json:"source"`
	Base           string              `json:"base"`
	Commits        []gitservice.Commit `json:"commits"`
	Applied        int                 `json:"applied"`
	Pending        int                 `json:"pending"`
	Available      int                 `json:"available"`
	ExcludedMerges int                 `json:"excluded_merge_commits"`
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
	excludedMerges, err := service.MergeCount(ctx, opts.base, opts.source)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "count excluded source merges")
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
	result := compareResult{Source: opts.source, Base: opts.base, Commits: commits, ExcludedMerges: excludedMerges}
	for _, commit := range commits {
		if commit.Applied {
			result.Applied++
		} else {
			result.Pending++
			result.Available++
		}
	}
	if opts.json {
		return writeJSON(a.IO.Out, result)
	}
	a.printCompare(result, !opts.noColor && isTerminal(a.IO.Out))
	return nil
}

func (a *Application) printCompare(result compareResult, color bool) {
	bold, green, yellow, cyan, gray, reset := "", "", "", "", "", ""
	if color {
		bold, green, yellow, cyan, gray, reset = "\x1b[1m", "\x1b[32m", "\x1b[33m", "\x1b[36m", "\x1b[90m", "\x1b[0m"
	}
	renderer := ui.Renderer{Writer: a.IO.Out, Color: color}
	renderer.Command("compare")
	renderer.Section("남은 작업")
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
	if result.ExcludedMerges > 0 {
		renderer.Warning("Excluded", fmt.Sprintf("work merge %d개와 side-parent 경로는 first-parent 후보에 포함되지 않습니다", result.ExcludedMerges))
	}
	if result.Pending > 0 {
		renderer.Pending("Pending", fmt.Sprintf("%d개", result.Pending))
		renderer.Field("Available", fmt.Sprintf("%d개", result.Available))
		if result.Available > 0 {
			renderer.Next("kit pick <new-branch>")
		}
	} else {
		renderer.Success("Pending", "없음")
	}
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

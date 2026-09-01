package app

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"kit/internal/clierror"
	"kit/internal/procutil"
)

type processKillResult struct {
	Process procutil.Process `json:"process"`
	Signal  string           `json:"signal"`
	Killed  bool             `json:"killed"`
}

func (a *Application) processCommand(ctx context.Context, global globalOptions, args []string) error {
	var err error
	global, args, err = parseLeadingGlobals(global, args)
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printProcessHelp(a.IO.Out)
		return nil
	}
	if args[0] == "kill" {
		return a.processKill(ctx, global, args[1:])
	}
	if args[0] == "info" {
		args = args[1:]
	}
	global, remaining, err := parseAllGlobals(global, args)
	if err != nil {
		return err
	}
	if len(remaining) != 1 {
		return clierror.New(clierror.Usage, "process requires exactly one PID")
	}
	pid, err := parsePID(remaining[0])
	if err != nil {
		return err
	}
	info, err := procutil.Info(ctx, nil, pid)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "inspect PID %d", pid)
	}
	if global.json {
		return writeJSON(a.IO.Out, info)
	}
	r := a.renderer(global)
	r.Command("process")
	r.Field("PID", strconv.Itoa(info.PID))
	r.Field("PPID", strconv.Itoa(info.PPID))
	r.Field("User", info.User)
	r.Field("Elapsed", info.Elapsed)
	r.Field("Command", info.Command)
	return nil
}

func (a *Application) processKill(ctx context.Context, global globalOptions, args []string) error {
	opts := signalOptions{}
	positionals := []string{}
	for len(args) > 0 {
		if consumed, err := parseGlobal(&global, args); err != nil {
			return err
		} else if consumed > 0 {
			args = args[consumed:]
			continue
		}
		switch {
		case args[0] == "--signal":
			if len(args) < 2 || args[1] == "" {
				return clierror.New(clierror.Usage, "--signal requires a signal name")
			}
			opts.signal, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--signal="):
			opts.signal, args = strings.TrimPrefix(args[0], "--signal="), args[1:]
		case strings.HasPrefix(args[0], "-"):
			return clierror.New(clierror.Usage, "unknown process kill option %q", args[0])
		default:
			positionals, args = append(positionals, args[0]), args[1:]
		}
	}
	if len(positionals) != 1 {
		return clierror.New(clierror.Usage, "process kill requires exactly one PID")
	}
	if global.json && !global.yes {
		return clierror.New(clierror.Usage, "process kill --json requires --yes")
	}
	pid, err := parsePID(positionals[0])
	if err != nil {
		return err
	}
	signal, signalName, err := procutil.ParseSignal(opts.signal)
	if err != nil {
		return clierror.Wrap(clierror.Usage, err, "invalid signal")
	}
	info, err := procutil.Info(ctx, nil, pid)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "inspect PID %d", pid)
	}
	if err := procutil.ValidateTarget(pid); err != nil {
		return clierror.Wrap(clierror.Conflict, err, "refuse process cleanup")
	}
	if !global.yes {
		ok, err := confirm(a.IO.In, a.IO.Out, fmt.Sprintf("PID %d (%s)에 SIG%s를 보내시겠습니까? [y/N] ", pid, info.Command, signalName))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}
	if err := procutil.Signal(pid, signal); err != nil {
		return clierror.Wrap(clierror.Failure, err, "signal PID %d", pid)
	}
	result := processKillResult{Process: info, Signal: signalName, Killed: true}
	if global.json {
		return writeJSON(a.IO.Out, result)
	}
	r := a.renderer(global)
	r.Command("process kill")
	r.Success("PID", strconv.Itoa(pid))
	r.Success("Signal", "SIG"+signalName)
	return nil
}

func parsePID(value string) (int, error) {
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return 0, clierror.New(clierror.Usage, "PID must be a positive integer")
	}
	return pid, nil
}

func printProcessHelp(w io.Writer) {
	fmt.Fprint(w, `Usage:
  kit process <pid>
  kit process info <pid>
  kit process kill <pid> [options]

Inspect or signal a local process.

Options:
  --signal <name>  Signal for kill (TERM, KILL, INT, HUP, QUIT; default TERM)
  --json           Print machine-readable output
  --yes            Skip kill confirmation
  --no-color       Disable ANSI colors
`)
}

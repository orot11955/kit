package app

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"kit/internal/clierror"
	"kit/internal/procutil"
)

type signalOptions struct {
	signal string
}

type portResult struct {
	Port      int                 `json:"port"`
	Listeners []procutil.Listener `json:"listeners"`
}

type portKillResult struct {
	Port      int                 `json:"port"`
	Signal    string              `json:"signal"`
	Processes []procutil.Listener `json:"processes"`
	Killed    bool                `json:"killed"`
}

type processKillResult struct {
	Process procutil.Process `json:"process"`
	Signal  string           `json:"signal"`
	Killed  bool             `json:"killed"`
}

func (a *Application) portCommand(ctx context.Context, global globalOptions, args []string) error {
	var err error
	global, args, err = parseLeadingGlobals(global, args)
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printPortHelp(a.IO.Out)
		return nil
	}
	if args[0] == "kill" {
		return a.portKill(ctx, global, args[1:])
	}
	if args[0] == "list" {
		args = args[1:]
	}
	global, remaining, err := parseAllGlobals(global, args)
	if err != nil {
		return err
	}
	if len(remaining) != 1 {
		return clierror.New(clierror.Usage, "port requires exactly one port number")
	}
	port, err := parsePort(remaining[0])
	if err != nil {
		return err
	}
	listeners, err := procutil.Listeners(ctx, nil, port)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "inspect port %d", port)
	}
	result := portResult{Port: port, Listeners: listeners}
	if global.json {
		return writeJSON(a.IO.Out, result)
	}
	r := a.renderer(global)
	r.Command("port")
	r.Field("Port", strconv.Itoa(port))
	if len(listeners) == 0 {
		r.Success("Listener", "없음")
		return nil
	}
	for _, item := range listeners {
		user := item.User
		if user == "" {
			user = "?"
		}
		r.Field("Listener", fmt.Sprintf("PID %d · %s · %s · %s · %s", item.PID, item.Command, user, item.Protocol, item.Address))
	}
	return nil
}

func (a *Application) portKill(ctx context.Context, global globalOptions, args []string) error {
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
			return clierror.New(clierror.Usage, "unknown port kill option %q", args[0])
		default:
			positionals, args = append(positionals, args[0]), args[1:]
		}
	}
	if len(positionals) != 1 {
		return clierror.New(clierror.Usage, "port kill requires exactly one port number")
	}
	if global.json && !global.yes {
		return clierror.New(clierror.Usage, "port kill --json requires --yes")
	}
	port, err := parsePort(positionals[0])
	if err != nil {
		return err
	}
	signal, signalName, err := procutil.ParseSignal(opts.signal)
	if err != nil {
		return clierror.Wrap(clierror.Usage, err, "invalid signal")
	}
	listeners, err := procutil.Listeners(ctx, nil, port)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "inspect port %d", port)
	}
	processes := uniqueListenerProcesses(listeners)
	if len(processes) == 0 {
		result := portKillResult{Port: port, Signal: signalName, Processes: []procutil.Listener{}, Killed: false}
		if global.json {
			return writeJSON(a.IO.Out, result)
		}
		a.renderer(global).Success("Listener", fmt.Sprintf("port %d에 종료할 listener가 없습니다", port))
		return nil
	}
	for _, item := range processes {
		if err := procutil.ValidateTarget(item.PID); err != nil {
			return clierror.Wrap(clierror.Conflict, err, "refuse port %d cleanup", port)
		}
	}
	if !global.yes {
		for _, item := range processes {
			fmt.Fprintf(a.IO.Out, "PID %d  %s  %s\n", item.PID, item.Command, item.Address)
		}
		ok, err := confirm(a.IO.In, a.IO.Out, fmt.Sprintf("port %d의 %d개 process에 SIG%s를 보내시겠습니까? [y/N] ", port, len(processes), signalName))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(a.IO.Out, "취소되었습니다.")
			return nil
		}
	}
	for _, item := range processes {
		if err := procutil.Signal(item.PID, signal); err != nil {
			return clierror.Wrap(clierror.Failure, err, "signal port %d listener PID %d", port, item.PID)
		}
	}
	result := portKillResult{Port: port, Signal: signalName, Processes: processes, Killed: true}
	if global.json {
		return writeJSON(a.IO.Out, result)
	}
	r := a.renderer(global)
	r.Command("port kill")
	r.Success("Signal", "SIG"+signalName)
	for _, item := range processes {
		r.Success("PID", fmt.Sprintf("%d · %s", item.PID, item.Command))
	}
	return nil
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

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, clierror.New(clierror.Usage, "port must be an integer between 1 and 65535")
	}
	return port, nil
}

func parsePID(value string) (int, error) {
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return 0, clierror.New(clierror.Usage, "PID must be a positive integer")
	}
	return pid, nil
}

func uniqueListenerProcesses(listeners []procutil.Listener) []procutil.Listener {
	byPID := make(map[int]procutil.Listener)
	for _, item := range listeners {
		if _, exists := byPID[item.PID]; !exists {
			byPID[item.PID] = item
		}
	}
	pids := make([]int, 0, len(byPID))
	for pid := range byPID {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	result := make([]procutil.Listener, 0, len(pids))
	for _, pid := range pids {
		result = append(result, byPID[pid])
	}
	return result
}

func printPortHelp(w io.Writer) {
	fmt.Fprint(w, `Usage:
  kit port <port>
  kit port list <port>
  kit port kill <port> [options]

Inspect TCP listeners and UDP bindings on a local port.

Options:
  --signal <name>  Signal for kill (TERM, KILL, INT, HUP, QUIT; default TERM)
  --json           Print machine-readable output
  --yes            Skip kill confirmation
  --no-color       Disable ANSI colors
`)
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

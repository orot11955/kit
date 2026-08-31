package procutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type Listener struct {
	PID      int    `json:"pid"`
	Command  string `json:"command"`
	User     string `json:"user,omitempty"`
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
}

type Process struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	User    string `json:"user"`
	Elapsed string `json:"elapsed"`
	Command string `json:"command"`
}

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
	LookPath(string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return stdout.Bytes(), &CommandError{Name: name, Args: append([]string(nil), args...), Message: message, Cause: err}
	}
	return stdout.Bytes(), nil
}

func (ExecRunner) LookPath(name string) (string, error) { return exec.LookPath(name) }

type CommandError struct {
	Name    string
	Args    []string
	Message string
	Cause   error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("%s %s: %s", e.Name, strings.Join(e.Args, " "), e.Message)
}

func (e *CommandError) Unwrap() error { return e.Cause }

func (e *CommandError) ExitCode() int {
	var exitErr *exec.ExitError
	if errors.As(e.Cause, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func Listeners(ctx context.Context, runner Runner, port int) ([]Listener, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port must be between 1 and 65535")
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	if _, err := runner.LookPath("lsof"); err != nil {
		return nil, errors.New("lsof is required for port inspection; on Ubuntu install it with 'sudo apt install lsof'")
	}

	var result []Listener
	for _, query := range [][]string{
		{"-nP", "-iTCP:" + strconv.Itoa(port), "-sTCP:LISTEN", "-FpcLPn"},
		{"-nP", "-iUDP:" + strconv.Itoa(port), "-FpcLPn"},
	} {
		out, err := runner.Run(ctx, "lsof", query...)
		if err != nil {
			var commandErr *CommandError
			if errors.As(err, &commandErr) && commandErr.ExitCode() == 1 {
				continue
			}
			return nil, err
		}
		result = append(result, parseLSOF(out)...)
	}

	seen := make(map[string]struct{}, len(result))
	filtered := result[:0]
	for _, item := range result {
		key := fmt.Sprintf("%d\x00%s\x00%s", item.PID, item.Protocol, item.Address)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		filtered = append(filtered, item)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].PID != filtered[j].PID {
			return filtered[i].PID < filtered[j].PID
		}
		if filtered[i].Protocol != filtered[j].Protocol {
			return filtered[i].Protocol < filtered[j].Protocol
		}
		return filtered[i].Address < filtered[j].Address
	})
	return filtered, nil
}

func parseLSOF(data []byte) []Listener {
	var result []Listener
	current := Listener{}
	flushAddress := func(address string) {
		if current.PID <= 0 || address == "" {
			return
		}
		item := current
		item.Address = address
		result = append(result, item)
	}
	for _, raw := range strings.Split(strings.ReplaceAll(string(data), "\x00", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if len(line) < 2 {
			continue
		}
		value := line[1:]
		switch line[0] {
		case 'p':
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 0 {
				current = Listener{}
				continue
			}
			current = Listener{PID: pid}
		case 'c':
			current.Command = value
		case 'L':
			current.User = value
		case 'P':
			current.Protocol = strings.ToUpper(value)
		case 'n':
			flushAddress(value)
		}
	}
	return result
}

func Info(ctx context.Context, runner Runner, pid int) (Process, error) {
	if pid <= 0 {
		return Process{}, errors.New("PID must be a positive integer")
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	if _, err := runner.LookPath("ps"); err != nil {
		return Process{}, errors.New("ps is required for process inspection")
	}
	out, err := runner.Run(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "pid=", "-o", "ppid=", "-o", "user=", "-o", "etime=", "-o", "command=")
	if err != nil {
		return Process{}, err
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return Process{}, fmt.Errorf("process %d was not found", pid)
	}
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return Process{}, fmt.Errorf("unexpected ps output for PID %d", pid)
	}
	parsedPID, err := strconv.Atoi(fields[0])
	if err != nil {
		return Process{}, fmt.Errorf("parse process PID: %w", err)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return Process{}, fmt.Errorf("parse process PPID: %w", err)
	}
	return Process{
		PID: parsedPID, PPID: ppid, User: fields[2], Elapsed: fields[3],
		Command: strings.Join(fields[4:], " "),
	}, nil
}

func ParseSignal(value string) (syscall.Signal, string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "SIG")
	if value == "" {
		value = "TERM"
	}
	switch value {
	case "TERM":
		return syscall.SIGTERM, "TERM", nil
	case "KILL":
		return syscall.SIGKILL, "KILL", nil
	case "INT":
		return syscall.SIGINT, "INT", nil
	case "HUP":
		return syscall.SIGHUP, "HUP", nil
	case "QUIT":
		return syscall.SIGQUIT, "QUIT", nil
	default:
		return 0, "", fmt.Errorf("unsupported signal %q; use TERM, KILL, INT, HUP, or QUIT", value)
	}
}

func ValidateTarget(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("refusing to signal protected PID %d", pid)
	}
	if pid == os.Getpid() {
		return errors.New("refusing to signal the current kit process")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return fmt.Errorf("process %d is unavailable or cannot be signaled: %w", pid, err)
	}
	return nil
}

func Signal(pid int, signal syscall.Signal) error {
	if err := ValidateTarget(pid); err != nil {
		return err
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(signal); err != nil {
		return fmt.Errorf("signal PID %d: %w", pid, err)
	}
	return nil
}

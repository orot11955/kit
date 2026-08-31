package git

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// RemoteBranchHash reads one remote branch without updating local or
// remote-tracking refs. The bool result reports whether the branch exists.
func (s Service) RemoteBranchHash(ctx context.Context, remote, branch string) (string, bool, error) {
	if strings.TrimSpace(remote) == "" || strings.HasPrefix(remote, "-") {
		return "", false, fmt.Errorf("invalid remote %q", remote)
	}
	if err := s.ValidateBranchName(ctx, branch); err != nil {
		return "", false, err
	}
	out, err := s.run(ctx, "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		return "", false, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", false, nil
	}
	if len(fields) != 2 || fields[1] != "refs/heads/"+branch || len(fields[0]) != 40 {
		return "", false, fmt.Errorf("unexpected ls-remote response for %s/%s", remote, branch)
	}
	return strings.ToLower(fields[0]), true, nil
}

// TraceRunner reports Git invocations without printing stdin or known token
// values. It is intended for --verbose diagnostics and preserves the wrapped
// runner's behavior.
type TraceRunner struct {
	Base   Runner
	Writer io.Writer
}

func (r TraceRunner) base() Runner {
	if r.Base == nil {
		return ExecRunner{}
	}
	return r.Base
}

func (r TraceRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	r.write(args, 0, false)
	return r.base().Run(ctx, dir, args...)
}

func (r TraceRunner) RunInput(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error) {
	r.write(args, len(input), true)
	return r.base().RunInput(ctx, dir, input, args...)
}

func (r TraceRunner) write(args []string, inputBytes int, hasInput bool) {
	if r.Writer == nil {
		return
	}
	command := SafeCommand(args)
	if hasInput {
		fmt.Fprintf(r.Writer, "+ git %s <stdin:%d bytes>\n", command, inputBytes)
		return
	}
	fmt.Fprintf(r.Writer, "+ git %s\n", command)
}

func SafeCommand(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		arg = redactGitError(arg)
		if strings.ContainsAny(arg, " \t\n\r\"'") {
			arg = strconv.Quote(arg)
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}

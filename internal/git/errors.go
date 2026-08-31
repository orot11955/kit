package git

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
)

var ErrConfigNotSet = errors.New("repository config is not set")

type CommandError struct {
	Args     []string
	Message  string
	ExitCode int
	cause    error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("git %s: %s", SafeCommand(e.Args), e.Message)
}

func (e *CommandError) Unwrap() error { return e.cause }

func commandFailure(args []string, stderr string, cause error) error {
	message := redactGitError(stderr)
	if message == "" && cause != nil {
		message = redactGitError(cause.Error())
	}
	code := -1
	var exitErr *exec.ExitError
	if errors.As(cause, &exitErr) {
		code = exitErr.ExitCode()
	}
	return &CommandError{Args: slices.Clone(args), Message: message, ExitCode: code, cause: cause}
}

func IsExitCode(err error, code int) bool {
	var commandErr *CommandError
	return errors.As(err, &commandErr) && commandErr.ExitCode == code
}

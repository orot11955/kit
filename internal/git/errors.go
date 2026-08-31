package git

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
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
	message = redactArgumentSecrets(message, args)
	code := -1
	var exitErr *exec.ExitError
	if errors.As(cause, &exitErr) {
		code = exitErr.ExitCode()
	}
	return &CommandError{Args: slices.Clone(args), Message: message, ExitCode: code, cause: cause}
}

func redactArgumentSecrets(message string, args []string) string {
	for _, arg := range args {
		for _, match := range gitQuerySecretPattern.FindAllString(arg, -1) {
			if _, secret, ok := strings.Cut(match, "="); ok && secret != "" {
				message = strings.ReplaceAll(message, secret, "<redacted>")
			}
		}
		if credentialURL := gitURLUserPattern.FindString(arg); credentialURL != "" {
			parts := strings.SplitN(credentialURL, "://", 2)
			if len(parts) == 2 {
				userinfo := strings.TrimSuffix(parts[1], "@")
				if userinfo != "" {
					message = strings.ReplaceAll(message, userinfo, "<redacted>")
					if _, password, ok := strings.Cut(userinfo, ":"); ok && password != "" {
						message = strings.ReplaceAll(message, password, "<redacted>")
					}
				}
			}
		}
	}
	return message
}

func IsExitCode(err error, code int) bool {
	var commandErr *CommandError
	return errors.As(err, &commandErr) && commandErr.ExitCode == code
}

package clierror

import "fmt"

const (
	Success   = 0
	Failure   = 1
	Usage     = 2
	Conflict  = 3
	Interrupt = 130
)

type Error struct {
	Code    int
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Message != "" && e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return fmt.Sprintf("exit code %d", e.Code)
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code int, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func Wrap(code int, err error, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Cause: err}
}

func Code(err error) int {
	if err == nil {
		return Success
	}
	if exitErr, ok := err.(*Error); ok {
		return exitErr.Code
	}
	return Failure
}

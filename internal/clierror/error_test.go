package clierror

import (
	"errors"
	"strings"
	"testing"
)

func TestWrapKeepsContextAndCauseInUserMessage(t *testing.T) {
	cause := errors.New("underlying failure")
	err := Wrap(Failure, cause, "operation failed")
	if !strings.Contains(err.Error(), "operation failed") || !strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("wrapped error lost context or cause: %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatal("wrapped error does not preserve its cause")
	}
}

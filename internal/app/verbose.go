package app

import (
	"context"
	"fmt"

	gitservice "kit/internal/git"
)

type verboseContextKey struct{}

func stripVerboseFlag(ctx context.Context, args []string) (context.Context, []string, bool) {
	filtered := make([]string, 0, len(args))
	verbose := false
	for _, arg := range args {
		if arg == "--verbose" {
			verbose = true
			continue
		}
		filtered = append(filtered, arg)
	}
	if verbose {
		ctx = context.WithValue(ctx, verboseContextKey{}, true)
	}
	return ctx, filtered, verbose
}

func verboseEnabled(ctx context.Context) bool {
	value, _ := ctx.Value(verboseContextKey{}).(bool)
	return value
}

func (a *Application) enableVerboseGit() func() {
	previous := a.Git
	a.Git = func(dir string) gitservice.Service {
		service := previous(dir)
		base := service.Runner
		if base == nil {
			base = gitservice.ExecRunner{}
		}
		service.Runner = gitservice.TraceRunner{Base: base, Writer: a.IO.ErrOut}
		return service
	}
	return func() { a.Git = previous }
}

func (a *Application) tracef(ctx context.Context, format string, args ...any) {
	if !verboseEnabled(ctx) || a.IO.ErrOut == nil {
		return
	}
	fmt.Fprintf(a.IO.ErrOut, "+ kit "+format+"\n", args...)
}

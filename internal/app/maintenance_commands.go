package app

import (
	"context"
	"fmt"

	"kit/internal/clierror"
	"kit/internal/update"
)

func (a *Application) version(global globalOptions, args []string) error {
	for len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Fprintln(a.IO.Out, "Usage: kit version [--json]")
			return nil
		}
		consumed, err := parseGlobal(&global, args)
		if err != nil {
			return err
		}
		if consumed == 0 {
			return clierror.New(clierror.Usage, "unknown version option %q", args[0])
		}
		args = args[consumed:]
	}
	if global.json {
		return writeJSON(a.IO.Out, a.Build)
	}
	fmt.Fprintf(a.IO.Out, "kit %s\ncommit: %s\nbuilt: %s\ntarget: %s\n", a.Build.Version, a.Build.Commit, a.Build.BuildDate, a.Build.Target)
	return nil
}

func (a *Application) update(ctx context.Context, global globalOptions, args []string) error {
	checkOnly := false
	rollback := false
	for len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Fprintln(a.IO.Out, "Usage: kit update [--check | --rollback] [--json]")
			return nil
		}
		switch args[0] {
		case "--check":
			checkOnly = true
			args = args[1:]
			continue
		case "--rollback":
			rollback = true
			args = args[1:]
			continue
		}
		consumed, err := parseGlobal(&global, args)
		if err != nil {
			return err
		}
		if consumed == 0 {
			return clierror.New(clierror.Usage, "unknown update option %q", args[0])
		}
		args = args[consumed:]
	}
	if checkOnly && rollback {
		return clierror.New(clierror.Usage, "--check and --rollback cannot be combined")
	}
	result, err := a.Update(ctx, update.Config{
		Current:    a.Build,
		Executable: a.ExecPath,
		CheckOnly:  checkOnly,
		Rollback:   rollback,
	})
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "update failed")
	}
	if global.json {
		return writeJSON(a.IO.Out, result)
	}
	if rollback {
		fmt.Fprintf(a.IO.Out, "kit %s → %s 롤백 완료\n", result.Current, result.Previous)
		return nil
	}
	if checkOnly {
		if result.UpdateAvailable {
			fmt.Fprintf(a.IO.Out, "업데이트 가능: kit %s → %s\n", result.Current, result.Latest)
		} else {
			fmt.Fprintf(a.IO.Out, "이미 최신 버전입니다: %s\n", result.Current)
		}
		return nil
	}
	if result.Updated {
		fmt.Fprintf(a.IO.Out, "kit %s → %s 업데이트 완료\n", result.Current, result.Latest)
	} else {
		fmt.Fprintf(a.IO.Out, "이미 최신 버전입니다: %s\n", result.Current)
	}
	return nil
}

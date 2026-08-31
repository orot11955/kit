package app

import (
	"context"
	"errors"
	"fmt"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/pickstate"
	"kit/internal/reviewstate"
)

func isRecoveryDoctorCommand(args []string) bool {
	for _, arg := range args {
		if arg == "--recovery" {
			return true
		}
	}
	return false
}

func (a *Application) doctorRecoveryCommand(ctx context.Context, global globalOptions, args []string) error {
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--recovery" {
			continue
		}
		if arg == "--network" {
			return clierror.New(clierror.Usage, "doctor --recovery and --network are separate diagnostics; run them independently")
		}
		filtered = append(filtered, arg)
	}
	for len(filtered) > 0 {
		if filtered[0] == "-h" || filtered[0] == "--help" {
			fmt.Fprintln(a.IO.Out, "Usage: kit doctor --recovery [--json] [--cwd <path>]")
			return nil
		}
		consumed, err := parseGlobal(&global, filtered)
		if err != nil {
			return err
		}
		if consumed == 0 {
			return clierror.New(clierror.Usage, "unknown doctor --recovery option %q", filtered[0])
		}
		filtered = filtered[consumed:]
	}

	service, err := a.validatedGit(ctx, global.cwd)
	if err != nil {
		return err
	}
	result := doctorResult{OK: true, Config: service.WorkflowConfig(ctx), Checks: []doctorCheck{}}
	appendRecoveryDoctorChecks(ctx, service, &result)
	if global.json {
		return writeJSON(a.IO.Out, result)
	}
	for _, check := range result.Checks {
		marker := "✓"
		if !check.OK {
			marker = "✗"
		}
		fmt.Fprintf(a.IO.Out, "%s %-14s %s\n", marker, check.Name, check.Message)
	}
	if !result.OK {
		return clierror.New(clierror.Failure, "doctor found interrupted or recovery state")
	}
	return nil
}

func appendRecoveryDoctorChecks(ctx context.Context, service gitservice.Service, result *doctorResult) {
	pick, err := pickstate.Load(ctx, service)
	switch {
	case errors.Is(err, pickstate.ErrNotFound):
		result.Checks = append(result.Checks, doctorCheck{Name: "pick state", OK: true, Message: "no interrupted kit pick"})
	case err != nil:
		result.OK = false
		result.Checks = append(result.Checks, doctorCheck{Name: "pick state", OK: false, Message: err.Error()})
	default:
		result.OK = false
		result.Checks = append(result.Checks, doctorCheck{
			Name: "pick state", OK: false,
			Message: fmt.Sprintf("%s paused at %d/%d; use kit pick --continue, --skip, or --abort", pick.TargetBranch, pick.Next, len(pick.Commits)),
		})
	}

	if inProgress, progressErr := service.CherryPickInProgress(ctx); progressErr != nil {
		result.OK = false
		result.Checks = append(result.Checks, doctorCheck{Name: "cherry-pick", OK: false, Message: progressErr.Error()})
	} else if inProgress {
		result.OK = false
		result.Checks = append(result.Checks, doctorCheck{Name: "cherry-pick", OK: false, Message: "Git CHERRY_PICK_HEAD exists; finish or abort the interrupted operation"})
	} else {
		result.Checks = append(result.Checks, doctorCheck{Name: "cherry-pick", OK: true, Message: "no Git cherry-pick in progress"})
	}

	appendRecoveryRefs(ctx, service, result, "recovery refs", "kit/recovery/", true)
	appendRecoveryRefs(ctx, service, result, "temporary refs", "kit/tmp/", true)
	appendRecoveryRefs(ctx, service, result, "backup refs", "kit/backup/", false)

	markers, markerErr := service.KitCreatedBranches(ctx)
	if markerErr != nil {
		result.OK = false
		result.Checks = append(result.Checks, doctorCheck{Name: "branch markers", OK: false, Message: markerErr.Error()})
	} else {
		stale := make([]string, 0)
		for _, branch := range markers {
			exists, existsErr := service.LocalBranchExists(ctx, branch)
			if existsErr != nil {
				result.OK = false
				result.Checks = append(result.Checks, doctorCheck{Name: "branch markers", OK: false, Message: existsErr.Error()})
				stale = nil
				break
			}
			if !exists {
				stale = append(stale, branch)
			}
		}
		if stale != nil {
			if len(stale) == 0 {
				result.Checks = append(result.Checks, doctorCheck{Name: "branch markers", OK: true, Message: fmt.Sprintf("%d active Kit branch marker(s), no stale markers", len(markers))})
			} else {
				result.OK = false
				result.Checks = append(result.Checks, doctorCheck{Name: "branch markers", OK: false, Message: fmt.Sprintf("%d stale marker(s); run kit branch-clean --delete after dry-run", len(stale))})
			}
		}
	}

	states, stateErr := reviewstate.List(ctx, service)
	if stateErr != nil {
		result.OK = false
		result.Checks = append(result.Checks, doctorCheck{Name: "review states", OK: false, Message: stateErr.Error()})
	} else {
		active := 0
		for _, state := range states {
			if state.Stage != reviewstate.StageCleaned && state.Stage != reviewstate.StageClosed {
				active++
			}
		}
		result.Checks = append(result.Checks, doctorCheck{Name: "review states", OK: true, Message: fmt.Sprintf("%d saved, %d active", len(states), active)})
	}
}

func appendRecoveryRefs(ctx context.Context, service gitservice.Service, result *doctorResult, name, prefix string, problematic bool) {
	refs, err := service.ListRefs(ctx, prefix)
	if err != nil {
		result.OK = false
		result.Checks = append(result.Checks, doctorCheck{Name: name, OK: false, Message: err.Error()})
		return
	}
	if len(refs) == 0 {
		result.Checks = append(result.Checks, doctorCheck{Name: name, OK: true, Message: "none"})
		return
	}
	if problematic {
		result.OK = false
		result.Checks = append(result.Checks, doctorCheck{Name: name, OK: false, Message: fmt.Sprintf("%d ref(s) remain; inspect with git branch --list '%s*'", len(refs), prefix)})
		return
	}
	result.Checks = append(result.Checks, doctorCheck{Name: name, OK: true, Message: fmt.Sprintf("%d retained backup ref(s)", len(refs))})
}

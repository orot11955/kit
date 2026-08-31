package app

import (
	"context"
	"fmt"
	"io"

	"kit/internal/clierror"
	"kit/internal/reviewstate"
)

type reviewFinishJSONResult struct {
	Finished bool              `json:"finished"`
	Review   reviewstate.State `json:"review"`
}

func isExtendedReviewCommand(global globalOptions, args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "finish":
		if global.json {
			return true
		}
		for _, arg := range args[1:] {
			if arg == "--json" {
				return true
			}
		}
	case "list":
		for _, arg := range args[1:] {
			if arg == "--refresh" {
				return true
			}
		}
	}
	return false
}

func (a *Application) reviewExtensionCommand(ctx context.Context, global globalOptions, args []string) error {
	if len(args) == 0 {
		return a.gitReview(ctx, global, args)
	}
	switch args[0] {
	case "finish":
		return a.reviewFinishJSON(ctx, global, args[1:])
	case "list":
		return a.reviewListRefresh(ctx, global, args[1:])
	default:
		return a.gitReview(ctx, global, args)
	}
}

func (a *Application) reviewFinishJSON(ctx context.Context, global globalOptions, args []string) error {
	branch, parsed, err := parseReviewBranchCommand(global, args, "finish")
	if err != nil {
		return err
	}
	if !parsed.json {
		return a.reviewFinish(ctx, parsed, reviewFinishOptions{branch: branch})
	}
	if !parsed.yes {
		return clierror.New(clierror.Usage, "review finish --json requires --yes")
	}

	service, err := a.reviewGitService(ctx, parsed, nil)
	if err != nil {
		return err
	}
	branch, err = resolveReviewBranch(ctx, service, branch)
	if err != nil {
		return err
	}

	originalOut := a.IO.Out
	a.IO.Out = io.Discard
	human := parsed
	human.json = false
	human.noColor = true
	human.yes = true
	finishErr := a.reviewFinish(ctx, human, reviewFinishOptions{branch: branch})
	a.IO.Out = originalOut
	if finishErr != nil {
		return finishErr
	}

	state, err := reviewstate.Load(ctx, service, branch)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "load finalized review state")
	}
	finished := state.Stage == reviewstate.StageSynced || state.Stage == reviewstate.StageCleaned
	return writeJSON(originalOut, reviewFinishJSONResult{Finished: finished, Review: state})
}

func (a *Application) reviewListRefresh(ctx context.Context, global globalOptions, args []string) error {
	refresh := false
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--refresh" {
			refresh = true
			continue
		}
		filtered = append(filtered, arg)
	}
	if !refresh {
		return a.reviewList(ctx, global)
	}
	parsed, remaining, err := parseAllGlobals(global, filtered)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return clierror.New(clierror.Usage, "review list --refresh accepts no arguments")
	}
	service, err := a.reviewGitService(ctx, parsed, nil)
	if err != nil {
		return err
	}
	states, err := reviewstate.List(ctx, service)
	if err != nil {
		return clierror.Wrap(clierror.Failure, err, "list review states")
	}
	refreshedAny := false
	for index, state := range states {
		if state.Stage == reviewstate.StageCleaned || state.ReviewNumber <= 0 {
			continue
		}
		updated, refreshed, refreshErr := a.refreshReviewState(ctx, service, state)
		if refreshErr != nil {
			return clierror.Wrap(clierror.Failure, refreshErr, "refresh %s PR #%d", state.Branch, state.ReviewNumber)
		}
		if refreshed {
			states[index] = updated
			refreshedAny = true
		}
	}
	if !parsed.json && !refreshedAny && len(states) > 0 {
		fmt.Fprintln(a.IO.ErrOut, "kit · review: provider refresh capability가 있는 active review가 없습니다.")
	}
	return a.printReviewStates(parsed, "review list --refresh", states, refreshedAny)
}

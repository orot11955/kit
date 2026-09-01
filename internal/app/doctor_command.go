package app

import (
	"context"
	"fmt"
	"strings"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
)

type doctorResult struct {
	OK        bool                      `json:"ok"`
	Config    gitservice.WorkflowConfig `json:"config"`
	Provider  string                    `json:"provider"`
	RemoteURL string                    `json:"remote_url,omitempty"`
	Checks    []doctorCheck             `json:"checks"`
}

type doctorCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func (a *Application) doctor(ctx context.Context, global globalOptions, args []string) error {
	for len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Fprintln(a.IO.Out, "Usage: kit doctor [--json] [--cwd <path>]")
			return nil
		}
		consumed, err := parseGlobal(&global, args)
		if err != nil {
			return err
		}
		if consumed == 0 {
			return clierror.New(clierror.Usage, "unknown doctor option %q", args[0])
		}
		args = args[consumed:]
	}
	service, err := a.validatedGit(ctx, global.cwd)
	if err != nil {
		return err
	}
	config := service.WorkflowConfig(ctx)
	result := doctorResult{OK: true, Config: config}
	for _, branch := range []struct{ role, name string }{{"stable", config.Stable}, {"base", config.Base}, {"source", config.Source}} {
		err := service.VerifyRevision(ctx, branch.name)
		check := doctorCheck{Name: branch.role + " branch", OK: err == nil, Message: branch.name}
		if err != nil {
			check.Message = fmt.Sprintf("%s: %v", branch.name, err)
			result.OK = false
		}
		result.Checks = append(result.Checks, check)
	}
	if remoteURL, remoteErr := service.RemoteURL(ctx, config.Remote); remoteErr == nil {
		repository := hosting.Resolve(config.Provider, remoteURL)
		result.RemoteURL, result.Provider = repository.Remote, repository.Provider
		result.Checks = append(result.Checks, doctorCheck{Name: "remote", OK: true, Message: config.Remote + " → " + repository.Remote})
	} else {
		result.Provider = strings.ToLower(config.Provider)
		result.Checks = append(result.Checks, doctorCheck{Name: "remote", OK: false, Message: remoteErr.Error()})
		result.OK = false
	}
	if config.PushRemote != "" {
		if pushURL, pushErr := service.RemoteURL(ctx, config.PushRemote); pushErr == nil {
			result.Checks = append(result.Checks, doctorCheck{Name: "push remote", OK: true, Message: config.PushRemote + " → " + pushURL})
		} else {
			result.Checks = append(result.Checks, doctorCheck{Name: "push remote", OK: false, Message: pushErr.Error()})
			result.OK = false
		}
	}
	if _, err := service.IsAncestor(ctx, config.Base, config.Source); err == nil {
		synced, _ := service.IsAncestor(ctx, config.Base, config.Source)
		message := "source contains base"
		if !synced {
			message = "source is stale; run kit sync"
			result.OK = false
		}
		result.Checks = append(result.Checks, doctorCheck{Name: "work sync", OK: synced, Message: message})
	}
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
		return clierror.New(clierror.Failure, "doctor found workflow problems")
	}
	return nil
}

package app

import (
	"context"
	"fmt"
	"strings"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
)

func (a *Application) configCommand(ctx context.Context, global globalOptions, args []string) error {
	var err error
	global, args, err = parseLeadingGlobals(global, args)
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(a.IO.Out, `Usage: kit config <command>

Repository-local commands:
  init                 Write the default Git workflow settings
  list                 Show the resolved Git workflow settings
  get <key>            Read one setting
  set <key> <value>    Write one setting
  unset <key>          Remove one setting

Keys:
  git.provider  git.remote  git.stable  git.base  git.source
  git.allow-insecure-http
`)
		return nil
	}
	command, rest := args[0], args[1:]
	filtered := rest[:0]
	for len(rest) > 0 {
		if consumed, err := parseGlobal(&global, rest); err != nil {
			return err
		} else if consumed > 0 {
			rest = rest[consumed:]
			continue
		}
		if rest[0] == "--scope" {
			if len(rest) < 2 || rest[1] != "repo" {
				return clierror.New(clierror.Usage, "only --scope repo is currently supported")
			}
			rest = rest[2:]
			continue
		}
		filtered = append(filtered, rest[0])
		rest = rest[1:]
	}
	service, err := a.validatedGit(ctx, global.cwd)
	if err != nil {
		return err
	}
	switch command {
	case "init":
		if len(filtered) != 0 {
			return clierror.New(clierror.Usage, "config init accepts no arguments")
		}
		config, err := service.InitializeWorkflowConfig(ctx)
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "initialize repository config")
		}
		return a.printWorkflowConfig(config, global.json)
	case "list":
		if len(filtered) != 0 {
			return clierror.New(clierror.Usage, "config list accepts no arguments")
		}
		return a.printWorkflowConfig(service.WorkflowConfig(ctx), global.json)
	case "get":
		if len(filtered) != 1 {
			return clierror.New(clierror.Usage, "config get requires one key")
		}
		value, err := service.ConfigGet(ctx, filtered[0])
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "read repository config")
		}
		if global.json {
			return writeJSON(a.IO.Out, map[string]string{filtered[0]: value})
		}
		fmt.Fprintln(a.IO.Out, value)
		return nil
	case "set":
		if len(filtered) != 2 {
			return clierror.New(clierror.Usage, "config set requires a key and value")
		}
		if err := service.ConfigSet(ctx, filtered[0], filtered[1]); err != nil {
			return clierror.Wrap(clierror.Failure, err, "write repository config")
		}
		fmt.Fprintf(a.IO.Out, "%s=%s\n", filtered[0], filtered[1])
		return nil
	case "unset":
		if len(filtered) != 1 {
			return clierror.New(clierror.Usage, "config unset requires one key")
		}
		if err := service.ConfigUnset(ctx, filtered[0]); err != nil {
			return clierror.Wrap(clierror.Failure, err, "remove repository config")
		}
		return nil
	default:
		return clierror.New(clierror.Usage, "unknown config command %q", command)
	}
}

func (a *Application) printWorkflowConfig(config gitservice.WorkflowConfig, jsonOutput bool) error {
	if jsonOutput {
		return writeJSON(a.IO.Out, config)
	}
	fmt.Fprintf(a.IO.Out, "git.provider=%s\ngit.remote=%s\ngit.stable=%s\ngit.base=%s\ngit.source=%s\ngit.allow-insecure-http=%t\n", config.Provider, config.Remote, config.Stable, config.Base, config.Source, config.AllowInsecureHTTP)
	return nil
}

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

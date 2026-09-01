package app

import (
	"context"
	"fmt"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
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
  bootstrap            Initialize config and missing local workflow branches from the remote
  list                 Show the resolved Git workflow settings
  get <key>            Read one setting
  set <key> <value>    Write one setting
  unset <key>          Remove one setting

Keys:
  git.provider  git.remote  git.push-remote  git.stable  git.base  git.source
  git.allow-insecure-http

Fork workflow:
  Keep git.remote pointed at the upstream/base repository and set
  git.push-remote to the fork remote used for review branches.
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
	case "bootstrap":
		if len(filtered) != 0 {
			return clierror.New(clierror.Usage, "config bootstrap accepts no arguments")
		}
		config, err := service.InitializeWorkflowConfig(ctx)
		if err != nil {
			return clierror.Wrap(clierror.Failure, err, "initialize repository config")
		}
		result, err := bootstrapWorkflow(ctx, service, config)
		if err != nil {
			return err
		}
		if global.json {
			return writeJSON(a.IO.Out, result)
		}
		renderer := a.renderer(global)
		renderer.Command("config bootstrap")
		renderer.Success("Remote", config.Remote+" fetched")
		for _, branch := range result.Created {
			renderer.Success("Created", branch)
		}
		for _, branch := range result.Existing {
			renderer.Field("Existing", branch)
		}
		renderer.Next("kit doctor")
		return nil
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
	fmt.Fprintf(a.IO.Out, "git.provider=%s\ngit.remote=%s\ngit.push-remote=%s\ngit.stable=%s\ngit.base=%s\ngit.source=%s\ngit.allow-insecure-http=%t\n", config.Provider, config.Remote, config.PushRemote, config.Stable, config.Base, config.Source, config.AllowInsecureHTTP)
	return nil
}

type bootstrapResult struct {
	Config   gitservice.WorkflowConfig `json:"config"`
	Created  []string                  `json:"created"`
	Existing []string                  `json:"existing"`
}

func bootstrapWorkflow(ctx context.Context, service gitservice.Service, config gitservice.WorkflowConfig) (bootstrapResult, error) {
	result := bootstrapResult{Config: config, Created: []string{}, Existing: []string{}}
	if config.Stable == config.Base || config.Stable == config.Source || config.Base == config.Source {
		return result, clierror.New(clierror.Conflict, "stable, base, and source branches must use distinct names")
	}
	if err := service.Fetch(ctx, config.Remote); err != nil {
		return result, clierror.Wrap(clierror.Failure, err, "fetch %s", config.Remote)
	}

	for _, branch := range []string{config.Stable, config.Base} {
		remoteRef := config.Remote + "/" + branch
		if err := service.VerifyRevision(ctx, remoteRef); err != nil {
			return result, clierror.Wrap(clierror.Failure, err, "remote workflow branch %q was not found", remoteRef)
		}
		exists, err := service.LocalBranchExists(ctx, branch)
		if err != nil {
			return result, clierror.Wrap(clierror.Failure, err, "check local branch %q", branch)
		}
		if exists {
			result.Existing = append(result.Existing, branch)
			continue
		}
		if err := service.CreateBranchAt(ctx, branch, remoteRef); err != nil {
			return result, clierror.Wrap(clierror.Failure, err, "create local %s from %s", branch, remoteRef)
		}
		result.Created = append(result.Created, branch)
	}

	remoteSource, err := service.RemoteTrackingBranchExists(ctx, config.Remote, config.Source)
	if err != nil {
		return result, clierror.Wrap(clierror.Failure, err, "check remote source branch")
	}
	if remoteSource {
		return result, clierror.New(clierror.Conflict, "%s/%s exists, but the configured source branch is a local-only queue; rename or remove the remote branch before bootstrap", config.Remote, config.Source)
	}

	sourceExists, err := service.LocalBranchExists(ctx, config.Source)
	if err != nil {
		return result, clierror.Wrap(clierror.Failure, err, "check local source branch %q", config.Source)
	}
	if !sourceExists {
		if err := service.CreateBranchAt(ctx, config.Source, config.Base); err != nil {
			return result, clierror.Wrap(clierror.Failure, err, "create local source branch %q", config.Source)
		}
		result.Created = append(result.Created, config.Source)
		return result, nil
	}
	result.Existing = append(result.Existing, config.Source)
	synced, err := service.IsAncestor(ctx, config.Base, config.Source)
	if err != nil {
		return result, clierror.Wrap(clierror.Failure, err, "check existing source branch")
	}
	if !synced {
		return result, clierror.New(clierror.Conflict, "existing source branch %s does not contain %s; run kit sync instead of overwriting it", config.Source, config.Base)
	}
	return result, nil
}

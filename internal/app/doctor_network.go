package app

import (
	"context"
	"fmt"
	"strings"

	"kit/internal/clierror"
	"kit/internal/hosting"
	"kit/internal/review"
)

func (a *Application) doctorEnhanced(ctx context.Context, global globalOptions, args []string) error {
	network := false
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--network" {
			network = true
			continue
		}
		filtered = append(filtered, arg)
	}
	if !network {
		return a.doctor(ctx, global, filtered)
	}

	for len(filtered) > 0 {
		if filtered[0] == "-h" || filtered[0] == "--help" {
			fmt.Fprintln(a.IO.Out, "Usage: kit doctor [--network] [--verbose] [--json] [--cwd <path>]")
			return nil
		}
		consumed, err := parseGlobal(&global, filtered)
		if err != nil {
			return err
		}
		if consumed == 0 {
			return clierror.New(clierror.Usage, "unknown doctor option %q", filtered[0])
		}
		filtered = filtered[consumed:]
	}

	service, err := a.validatedGit(ctx, global.cwd)
	if err != nil {
		return err
	}
	config := service.WorkflowConfig(ctx)
	result := doctorResult{OK: true, Config: config}

	for _, branch := range []struct{ role, name string }{
		{"stable", config.Stable},
		{"base", config.Base},
		{"source", config.Source},
	} {
		err := service.VerifyRevision(ctx, branch.name)
		check := doctorCheck{Name: branch.role + " branch", OK: err == nil, Message: branch.name}
		if err != nil {
			check.Message = fmt.Sprintf("%s: %v", branch.name, err)
			result.OK = false
		}
		result.Checks = append(result.Checks, check)
	}

	var repository hosting.Repository
	if remoteURL, remoteErr := service.RemoteURL(ctx, config.Remote); remoteErr == nil {
		repository = hosting.Resolve(config.Provider, remoteURL)
		repository.AllowInsecureHTTP = config.AllowInsecureHTTP
		result.RemoteURL, result.Provider = repository.Remote, repository.Provider
		result.Checks = append(result.Checks, doctorCheck{Name: "remote", OK: true, Message: config.Remote + " → " + repository.Remote})
	} else {
		result.Provider = strings.ToLower(config.Provider)
		result.Checks = append(result.Checks, doctorCheck{Name: "remote", OK: false, Message: remoteErr.Error()})
		result.OK = false
	}

	if synced, syncErr := service.IsAncestor(ctx, config.Base, config.Source); syncErr == nil {
		message := "source contains base"
		if !synced {
			message = "source is stale; run kit sync"
			result.OK = false
		}
		result.Checks = append(result.Checks, doctorCheck{Name: "work sync", OK: synced, Message: message})
	} else {
		result.Checks = append(result.Checks, doctorCheck{Name: "work sync", OK: false, Message: syncErr.Error()})
		result.OK = false
	}

	for _, branch := range []struct{ role, name string }{
		{"remote stable", config.Stable},
		{"remote base", config.Base},
	} {
		a.tracef(ctx, "network git %s/%s", config.Remote, branch.name)
		hash, exists, remoteErr := service.RemoteBranchHash(ctx, config.Remote, branch.name)
		check := doctorCheck{Name: branch.role, OK: remoteErr == nil && exists}
		switch {
		case remoteErr != nil:
			check.Message = remoteErr.Error()
		case !exists:
			check.Message = config.Remote + "/" + branch.name + " not found"
		default:
			check.Message = config.Remote + "/" + branch.name + " @ " + shortDiagnosticHash(hash)
		}
		if !check.OK {
			result.OK = false
		}
		result.Checks = append(result.Checks, check)
	}

	a.tracef(ctx, "network git local-only source check %s/%s", config.Remote, config.Source)
	_, sourceRemoteExists, sourceRemoteErr := service.RemoteBranchHash(ctx, config.Remote, config.Source)
	sourceCheck := doctorCheck{Name: "remote source", OK: sourceRemoteErr == nil && !sourceRemoteExists}
	switch {
	case sourceRemoteErr != nil:
		sourceCheck.Message = sourceRemoteErr.Error()
	case sourceRemoteExists:
		sourceCheck.Message = fmt.Sprintf("%s/%s exists; source queue must remain local-only", config.Remote, config.Source)
	default:
		sourceCheck.Message = fmt.Sprintf("%s/%s absent as expected", config.Remote, config.Source)
	}
	if !sourceCheck.OK {
		result.OK = false
	}
	result.Checks = append(result.Checks, sourceCheck)

	apiCheck := doctorCheck{Name: "review API"}
	if repository.Host == "" {
		apiCheck.Message = "repository host could not be resolved"
	} else {
		reviewRepo, reviewErr := reviewRepository(ctx, service, config)
		if reviewErr != nil {
			apiCheck.Message = reviewErr.Error()
		} else {
			a.tracef(ctx, "network review ping %s %s/%s", reviewRepo.Provider, reviewRepo.Owner, reviewRepo.Name)
			client, clientErr := a.ReviewClient(reviewRepo)
			if clientErr != nil {
				apiCheck.Message = clientErr.Error()
			} else if pinger, ok := client.(review.Pinger); ok {
				if pingErr := pinger.Ping(ctx); pingErr != nil {
					apiCheck.Message = pingErr.Error()
				} else {
					apiCheck.OK = true
					apiCheck.Message = reviewRepo.Provider + " repository API reachable"
				}
			} else {
				apiCheck.OK = true
				apiCheck.Message = reviewRepo.Provider + " adapter has no health endpoint; credential initialization succeeded"
			}
		}
	}
	if !apiCheck.OK {
		result.OK = false
	}
	result.Checks = append(result.Checks, apiCheck)

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
		return clierror.New(clierror.Failure, "doctor found workflow or network problems")
	}
	return nil
}

func shortDiagnosticHash(hash string) string {
	if len(hash) > 10 {
		return hash[:10]
	}
	return hash
}

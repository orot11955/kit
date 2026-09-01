package app

import (
	"context"
	"fmt"
	"strings"

	"kit/internal/clierror"
	gitservice "kit/internal/git"
	"kit/internal/hosting"
	"kit/internal/review"
)

type reviewRepositories struct {
	Target      hosting.Repository
	Source      hosting.Repository
	PushRemote  string
	SourceOwner string
	Fork        bool
}

func resolveReviewRepositories(ctx context.Context, service gitservice.Service, config gitservice.WorkflowConfig) (reviewRepositories, error) {
	result := reviewRepositories{PushRemote: config.PushRemoteName()}
	targetURL, err := service.RemoteURL(ctx, config.Remote)
	if err != nil {
		return result, clierror.Wrap(clierror.Failure, err, "read review target remote %s", config.Remote)
	}
	result.Target = hosting.Resolve(config.Provider, targetURL)
	result.Target.AllowInsecureHTTP = config.AllowInsecureHTTP
	if result.Target.Provider != "gitea" && result.Target.Provider != "gitlab" && result.Target.Provider != "forgejo" {
		return result, clierror.New(clierror.Failure, "provider %q does not support automated reviews", result.Target.Provider)
	}
	if result.PushRemote == config.Remote {
		result.Source = result.Target
		return result, nil
	}

	sourceURL, err := service.RemoteURL(ctx, result.PushRemote)
	if err != nil {
		return result, clierror.Wrap(clierror.Failure, err, "read review source remote %s", result.PushRemote)
	}
	result.Source = hosting.Resolve(config.Provider, sourceURL)
	result.Source.AllowInsecureHTTP = config.AllowInsecureHTTP
	if result.Target.Provider != "gitea" || result.Source.Provider != "gitea" {
		return result, clierror.New(clierror.Failure, "cross-repository review currently supports Gitea only")
	}
	if !strings.EqualFold(result.Source.Host, result.Target.Host) {
		return result, clierror.New(clierror.Conflict, "push remote %s and review target remote %s must use the same Gitea host", result.PushRemote, config.Remote)
	}
	if result.Source.Owner == "" || result.Target.Owner == "" || result.Source.Name == "" || result.Target.Name == "" {
		return result, clierror.New(clierror.Failure, "fork review requires resolvable source and target repository coordinates")
	}
	if result.Source.Owner == result.Target.Owner && result.Source.Name == result.Target.Name {
		return result, nil
	}
	if strings.Contains(result.Source.Owner, "/") {
		return result, clierror.New(clierror.Failure, "Gitea fork source owner must be a single path segment")
	}
	result.Fork = true
	result.SourceOwner = result.Source.Owner
	return result, nil
}

func findOrCreateReview(ctx context.Context, client review.Client, repositories reviewRepositories, branch, target, title, description string, draft, removeSourceBranch bool) (review.Review, bool, error) {
	request := review.CreateRequest{
		SourceBranch:       branch,
		TargetBranch:       target,
		Title:              title,
		Description:        description,
		Draft:              draft,
		RemoveSourceBranch: removeSourceBranch,
	}
	if repositories.Fork {
		forkClient, ok := client.(review.ForkClient)
		if !ok {
			return review.Review{}, false, fmt.Errorf("provider does not support fork pull requests")
		}
		if existing, found, err := forkClient.FindOpenFrom(ctx, repositories.SourceOwner, branch, target); err != nil {
			return review.Review{}, false, err
		} else if found {
			return existing, true, nil
		}
		created, err := forkClient.CreateFrom(ctx, repositories.SourceOwner, request)
		return created, false, err
	}
	if finder, ok := client.(review.OpenFinder); ok {
		if existing, found, err := finder.FindOpen(ctx, branch, target); err != nil {
			return review.Review{}, false, err
		} else if found {
			return existing, true, nil
		}
	}
	created, err := client.Create(ctx, request)
	return created, false, err
}

func reviewURLMatchesRepository(reviewURL string, repository hosting.Repository) bool {
	if strings.TrimSpace(reviewURL) == "" || strings.TrimSpace(repository.WebURL) == "" {
		return false
	}
	base := strings.TrimSuffix(repository.WebURL, "/") + "/"
	return strings.HasPrefix(reviewURL, base)
}

package review

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

func (c *giteaClient) CreateFrom(ctx context.Context, sourceOwner string, request CreateRequest) (Review, error) {
	if err := validateForkOwner(sourceOwner); err != nil {
		return Review{}, err
	}
	if err := validateCreateRequest(request); err != nil {
		return Review{}, err
	}
	title := request.Title
	if request.Draft && !strings.HasPrefix(title, "WIP: ") {
		title = "WIP: " + title
	}
	payload := struct {
		Head  string `json:"head"`
		Base  string `json:"base"`
		Title string `json:"title"`
		Body  string `json:"body,omitempty"`
	}{sourceOwner + ":" + request.SourceBranch, request.TargetBranch, title, request.Description}
	var response giteaReview
	if err := c.doJSON(ctx, http.MethodPost, c.pullsEndpoint(), payload, &response, "Authorization"); err != nil {
		return Review{}, err
	}
	return c.mapAndValidate(response)
}

func (c *giteaClient) FindOpenFrom(ctx context.Context, sourceOwner, sourceBranch, targetBranch string) (Review, bool, error) {
	if err := validateForkOwner(sourceOwner); err != nil {
		return Review{}, false, err
	}
	if strings.TrimSpace(sourceBranch) == "" || strings.TrimSpace(targetBranch) == "" {
		return Review{}, false, errors.New("source and target branch are required")
	}
	query := url.Values{}
	query.Set("state", "open")
	query.Set("limit", "50")
	var response []giteaReview
	if err := c.doJSON(ctx, http.MethodGet, c.pullsEndpoint()+"?"+query.Encode(), nil, &response, "Authorization"); err != nil {
		return Review{}, false, err
	}
	var found Review
	for _, item := range response {
		mapped, err := c.mapAndValidate(item)
		if err != nil {
			return Review{}, false, err
		}
		if mapped.Status != StatusOpen || mapped.SourceBranch != sourceBranch || mapped.TargetBranch != targetBranch {
			continue
		}
		if giteaRefOwner(item.Head) != sourceOwner {
			continue
		}
		if found.Number != 0 {
			return Review{}, false, errors.New("multiple open Gitea pull requests match the same fork source and target branches")
		}
		found = mapped
	}
	return found, found.Number != 0, nil
}

func validateForkOwner(owner string) error {
	owner = strings.TrimSpace(owner)
	if owner == "" || strings.ContainsAny(owner, "/:\\@ \t\r\n") || owner == "." || owner == ".." {
		return errors.New("fork source owner is invalid")
	}
	return nil
}

func giteaRefOwner(ref giteaRef) string {
	label := strings.TrimSpace(ref.Name)
	separator := strings.LastIndex(label, ":")
	if separator <= 0 {
		return ""
	}
	return label[:separator]
}

var _ ForkClient = (*giteaClient)(nil)

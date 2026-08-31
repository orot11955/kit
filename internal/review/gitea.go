package review

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type giteaClient struct {
	apiClient
	owner      string
	repository string
}

type giteaRef struct {
	Name string `json:"label"`
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
}

type giteaReview struct {
	ID             int64      `json:"id"`
	Number         int64      `json:"number"`
	HTMLURL        string     `json:"html_url"`
	State          string     `json:"state"`
	Merged         bool       `json:"merged"`
	Title          string     `json:"title"`
	Head           giteaRef   `json:"head"`
	Base           giteaRef   `json:"base"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	MergedAt       *time.Time `json:"merged_at"`
}

func (c *giteaClient) pullsEndpoint() string {
	return c.baseURL + "/api/v1/repos/" + url.PathEscape(c.owner) + "/" + url.PathEscape(c.repository) + "/pulls"
}

func (c *giteaClient) Create(ctx context.Context, request CreateRequest) (Review, error) {
	if err := validateCreateRequest(request); err != nil {
		return Review{}, err
	}
	title := request.Title
	if request.Draft && !strings.HasPrefix(title, "WIP: ") {
		title = "WIP: " + title
	}
	// Gitea's create pull request payload has no portable draft field. Draft
	// intent is represented by the conventional WIP title prefix instead.
	payload := struct {
		Head  string `json:"head"`
		Base  string `json:"base"`
		Title string `json:"title"`
		Body  string `json:"body,omitempty"`
	}{request.SourceBranch, request.TargetBranch, title, request.Description}
	var response giteaReview
	if err := c.doJSON(ctx, http.MethodPost, c.pullsEndpoint(), payload, &response, "Authorization"); err != nil {
		return Review{}, err
	}
	return c.mapAndValidate(response)
}

func (c *giteaClient) FindOpen(ctx context.Context, sourceBranch, targetBranch string) (Review, bool, error) {
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
		if found.Number != 0 {
			return Review{}, false, errors.New("multiple open Gitea pull requests match the same source and target branches")
		}
		found = mapped
	}
	return found, found.Number != 0, nil
}

func (c *giteaClient) Get(ctx context.Context, number int64) (Review, error) {
	if number <= 0 {
		return Review{}, errors.New("review number must be positive")
	}
	var response giteaReview
	endpoint := c.pullsEndpoint() + "/" + strconv.FormatInt(number, 10)
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &response, "Authorization"); err != nil {
		return Review{}, err
	}
	return c.mapAndValidate(response)
}

func (c *giteaClient) mapAndValidate(item giteaReview) (Review, error) {
	mapped := mapGiteaReview(item)
	if err := c.validateReview(mapped); err != nil {
		return Review{}, err
	}
	return mapped, nil
}

func mapGiteaReview(item giteaReview) Review {
	status := StatusClosed
	if item.Merged || item.MergedAt != nil {
		status = StatusMerged
	} else if item.State == "open" {
		status = StatusOpen
	}
	sourceBranch := giteaBranchName(item.Head)
	targetBranch := giteaBranchName(item.Base)
	return Review{
		Provider:     "gitea",
		ID:           strconv.FormatInt(item.Number, 10),
		Number:       item.Number,
		URL:          item.HTMLURL,
		Status:       status,
		SourceBranch: sourceBranch,
		TargetBranch: targetBranch,
		Title:        item.Title,
		SourceSHA:    item.Head.SHA,
		MergeSHA:     item.MergeCommitSHA,
		MergedAt:     item.MergedAt,
	}
}

func giteaBranchName(branch giteaRef) string {
	if branch.Ref != "" && !strings.HasPrefix(branch.Ref, "refs/pull/") {
		return branch.Ref
	}
	name := branch.Name
	if separator := strings.LastIndex(name, ":"); separator >= 0 {
		name = name[separator+1:]
	}
	if name != "" {
		return name
	}
	return branch.Ref
}

var _ Client = (*giteaClient)(nil)
var _ OpenFinder = (*giteaClient)(nil)
var _ Getter = (*giteaClient)(nil)

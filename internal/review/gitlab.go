package review

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type gitLabClient struct {
	apiClient
	projectPath string
}

type gitLabReview struct {
	ID              int64      `json:"id"`
	IID             int64      `json:"iid"`
	WebURL          string     `json:"web_url"`
	State           string     `json:"state"`
	Title           string     `json:"title"`
	SourceBranch    string     `json:"source_branch"`
	TargetBranch    string     `json:"target_branch"`
	SHA             string     `json:"sha"`
	MergeCommitSHA  string     `json:"merge_commit_sha"`
	SquashCommitSHA string     `json:"squash_commit_sha"`
	MergedAt        *time.Time `json:"merged_at"`
}

func (c *gitLabClient) mergeRequestsEndpoint() string {
	return c.baseURL + "/api/v4/projects/" + url.PathEscape(c.projectPath) + "/merge_requests"
}

func (c *gitLabClient) FindOpen(ctx context.Context, sourceBranch, targetBranch string) ([]Review, error) {
	return c.find(ctx, sourceBranch, targetBranch, "opened", true)
}

func (c *gitLabClient) Find(ctx context.Context, sourceBranch, targetBranch string) ([]Review, error) {
	return c.find(ctx, sourceBranch, targetBranch, "all", false)
}

func (c *gitLabClient) find(ctx context.Context, sourceBranch, targetBranch, state string, openOnly bool) ([]Review, error) {
	if err := validateFindRequest(sourceBranch, targetBranch); err != nil {
		return nil, err
	}
	reviews := make([]Review, 0, 1)
	for page := 1; page <= 20; page++ {
		query := url.Values{}
		query.Set("state", state)
		query.Set("source_branch", sourceBranch)
		query.Set("target_branch", targetBranch)
		query.Set("page", strconv.Itoa(page))
		query.Set("per_page", "100")
		var response []gitLabReview
		if err := c.doJSON(ctx, http.MethodGet, c.mergeRequestsEndpoint()+"?"+query.Encode(), nil, &response, "Private-Token"); err != nil {
			return nil, err
		}
		for _, item := range response {
			mapped := mapGitLabReview(item)
			if err := c.validateReview(mapped); err != nil {
				return nil, err
			}
			if (!openOnly || mapped.Status == StatusOpen) && mapped.SourceBranch == sourceBranch && mapped.TargetBranch == targetBranch {
				reviews = append(reviews, mapped)
			}
		}
		if len(response) < 100 {
			return reviews, nil
		}
	}
	return nil, errors.New("GitLab review lookup exceeded pagination limit")
}

func (c *gitLabClient) Create(ctx context.Context, request CreateRequest) (Review, error) {
	if err := validateCreateRequest(request); err != nil {
		return Review{}, err
	}
	payload := struct {
		SourceBranch       string `json:"source_branch"`
		TargetBranch       string `json:"target_branch"`
		Title              string `json:"title"`
		Description        string `json:"description,omitempty"`
		Draft              bool   `json:"draft,omitempty"`
		RemoveSourceBranch bool   `json:"remove_source_branch,omitempty"`
	}{request.SourceBranch, request.TargetBranch, request.Title, request.Description, request.Draft, request.RemoveSourceBranch}
	var response gitLabReview
	if err := c.doJSON(ctx, http.MethodPost, c.mergeRequestsEndpoint(), payload, &response, "Private-Token"); err != nil {
		return Review{}, err
	}
	mapped := mapGitLabReview(response)
	if err := c.validateReview(mapped); err != nil {
		return Review{}, err
	}
	return mapped, nil
}

func (c *gitLabClient) Get(ctx context.Context, id string) (Review, error) {
	id, err := positiveID(id)
	if err != nil {
		return Review{}, err
	}
	var response gitLabReview
	if err := c.doJSON(ctx, http.MethodGet, c.mergeRequestsEndpoint()+"/"+id, nil, &response, "Private-Token"); err != nil {
		return Review{}, err
	}
	mapped := mapGitLabReview(response)
	if err := c.validateReview(mapped); err != nil {
		return Review{}, err
	}
	return mapped, nil
}

func mapGitLabReview(item gitLabReview) Review {
	status := StatusClosed
	switch item.State {
	case "opened":
		status = StatusOpen
	case "merged":
		status = StatusMerged
	}
	mergeSHA := item.MergeCommitSHA
	if mergeSHA == "" {
		mergeSHA = item.SquashCommitSHA
	}
	number := item.IID
	return Review{
		Provider:     "gitlab",
		ID:           strconv.FormatInt(number, 10),
		Number:       number,
		URL:          item.WebURL,
		Status:       status,
		SourceBranch: item.SourceBranch,
		TargetBranch: item.TargetBranch,
		Title:        item.Title,
		SourceSHA:    item.SHA,
		MergeSHA:     mergeSHA,
		MergedAt:     item.MergedAt,
	}
}

var _ Client = (*gitLabClient)(nil)

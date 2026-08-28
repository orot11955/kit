package review

import (
	"context"
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

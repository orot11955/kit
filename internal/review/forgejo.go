package review

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type forgejoClient struct {
	apiClient
	owner      string
	repository string
}

type forgejoRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type forgejoReview struct {
	ID             int64      `json:"id"`
	Number         int64      `json:"number"`
	HTMLURL        string     `json:"html_url"`
	State          string     `json:"state"`
	Merged         bool       `json:"merged"`
	Title          string     `json:"title"`
	Head           forgejoRef `json:"head"`
	Base           forgejoRef `json:"base"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	MergedAt       *time.Time `json:"merged_at"`
}

func (c *forgejoClient) pullsEndpoint() string {
	return c.baseURL + "/api/v1/repos/" + url.PathEscape(c.owner) + "/" + url.PathEscape(c.repository) + "/pulls"
}

func (c *forgejoClient) Create(ctx context.Context, request CreateRequest) (Review, error) {
	if err := validateCreateRequest(request); err != nil {
		return Review{}, err
	}
	payload := struct {
		Head  string `json:"head"`
		Base  string `json:"base"`
		Title string `json:"title"`
		Body  string `json:"body,omitempty"`
		Draft bool   `json:"draft,omitempty"`
	}{request.SourceBranch, request.TargetBranch, request.Title, request.Description, request.Draft}
	var response forgejoReview
	if err := c.doJSON(ctx, http.MethodPost, c.pullsEndpoint(), payload, &response, "Authorization"); err != nil {
		return Review{}, err
	}
	mapped := mapForgejoReview(response)
	if err := c.validateReview(mapped); err != nil {
		return Review{}, err
	}
	return mapped, nil
}

func mapForgejoReview(item forgejoReview) Review {
	status := StatusClosed
	if item.Merged || item.MergedAt != nil {
		status = StatusMerged
	} else if item.State == "open" {
		status = StatusOpen
	}
	return Review{
		Provider:     "forgejo",
		ID:           strconv.FormatInt(item.Number, 10),
		Number:       item.Number,
		URL:          item.HTMLURL,
		Status:       status,
		SourceBranch: item.Head.Ref,
		TargetBranch: item.Base.Ref,
		Title:        item.Title,
		SourceSHA:    item.Head.SHA,
		MergeSHA:     item.MergeCommitSHA,
		MergedAt:     item.MergedAt,
	}
}

var _ Client = (*forgejoClient)(nil)

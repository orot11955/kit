package review

import (
	"context"
	"errors"
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

func (c *forgejoClient) FindOpen(ctx context.Context, sourceBranch, targetBranch string) ([]Review, error) {
	return c.find(ctx, sourceBranch, targetBranch, "open", true)
}

func (c *forgejoClient) Find(ctx context.Context, sourceBranch, targetBranch string) ([]Review, error) {
	return c.find(ctx, sourceBranch, targetBranch, "all", false)
}

func (c *forgejoClient) find(ctx context.Context, sourceBranch, targetBranch, state string, openOnly bool) ([]Review, error) {
	if err := validateFindRequest(sourceBranch, targetBranch); err != nil {
		return nil, err
	}
	reviews := make([]Review, 0, 1)
	for page := 1; page <= 20; page++ {
		query := url.Values{}
		query.Set("state", state)
		query.Set("page", strconv.Itoa(page))
		query.Set("limit", "50")
		var response []forgejoReview
		if err := c.doJSON(ctx, http.MethodGet, c.pullsEndpoint()+"?"+query.Encode(), nil, &response, "Authorization"); err != nil {
			return nil, err
		}
		for _, item := range response {
			mapped := mapForgejoReview(item)
			if err := c.validateReview(mapped); err != nil {
				return nil, err
			}
			if (!openOnly || mapped.Status == StatusOpen) && mapped.SourceBranch == sourceBranch && mapped.TargetBranch == targetBranch {
				reviews = append(reviews, mapped)
			}
		}
		if len(response) < 50 {
			return reviews, nil
		}
	}
	return nil, errors.New("Forgejo review lookup exceeded pagination limit")
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

func (c *forgejoClient) Get(ctx context.Context, id string) (Review, error) {
	id, err := positiveID(id)
	if err != nil {
		return Review{}, err
	}
	var response forgejoReview
	if err := c.doJSON(ctx, http.MethodGet, c.pullsEndpoint()+"/"+id, nil, &response, "Authorization"); err != nil {
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

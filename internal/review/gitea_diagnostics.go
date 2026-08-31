package review

import (
	"context"
	"net/http"
	"net/url"
)

func (c *giteaClient) repositoryEndpoint() string {
	return c.baseURL + "/api/v1/repos/" + url.PathEscape(c.owner) + "/" + url.PathEscape(c.repository)
}

func (c *giteaClient) Ping(ctx context.Context) error {
	var response struct {
		ID int64 `json:"id"`
	}
	return c.doJSON(ctx, http.MethodGet, c.repositoryEndpoint(), nil, &response, "Authorization")
}

var _ Pinger = (*giteaClient)(nil)

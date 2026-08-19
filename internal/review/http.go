package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	requestTimeout   = 15 * time.Second
	maxResponseBytes = 2 << 20
)

type apiClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func (c apiClient) validateReview(item Review) error {
	if err := validateReview(item); err != nil {
		return err
	}
	base, baseErr := url.Parse(c.baseURL)
	reviewURL, reviewErr := url.Parse(item.URL)
	if baseErr != nil || reviewErr != nil || reviewURL.Host != base.Host {
		return errors.New("review API response URL does not match repository host")
	}
	return nil
}

func secureHTTPClient(input *http.Client, origin *url.URL) *http.Client {
	client := &http.Client{}
	if input != nil {
		*client = *input
	}
	client.Timeout = requestTimeout
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many review API redirects")
		}
		if request.URL.Scheme != "https" || request.URL.Host != origin.Host {
			return errors.New("review API redirect changed HTTPS origin")
		}
		return nil
	}
	return client
}

func (c apiClient) doJSON(ctx context.Context, method, endpoint string, body any, output any, tokenHeader string) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode review API request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return errors.New("create review API request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "kit")
	if tokenHeader == "Authorization" {
		request.Header.Set(tokenHeader, "token "+c.token)
	} else {
		request.Header.Set(tokenHeader, c.token)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("review API request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes+1))
		return fmt.Errorf("review API request returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxResponseBytes {
		return errors.New("review API response exceeds size limit")
	}
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return errors.New("read review API response")
	}
	if len(data) > maxResponseBytes {
		return errors.New("review API response exceeds size limit")
	}
	if output == nil || len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, output); err != nil {
		return errors.New("decode review API response")
	}
	return nil
}

func validateCreateRequest(request CreateRequest) error {
	if request.SourceBranch == "" || request.TargetBranch == "" || strings.TrimSpace(request.Title) == "" {
		return errors.New("source branch, target branch, and title are required")
	}
	return nil
}

func validateFindRequest(sourceBranch, targetBranch string) error {
	if sourceBranch == "" || targetBranch == "" {
		return errors.New("source branch and target branch are required")
	}
	return nil
}

func positiveID(id string) (string, error) {
	if id == "" {
		return "", errors.New("review ID is required")
	}
	for _, character := range id {
		if character < '0' || character > '9' {
			return "", errors.New("review ID must be a positive integer")
		}
	}
	if strings.TrimLeft(id, "0") == "" {
		return "", errors.New("review ID must be a positive integer")
	}
	return id, nil
}

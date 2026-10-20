// Package registry implements the machine-scoped Registry client.
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	endpoint string
	reports  string
	key      string
	http     *http.Client
}

// NewClient takes the validated origin and machine key from config. Redirects
// are never followed, even on the same host: authentication belongs to one API.
func NewClient(origin, key string, timeout time.Duration) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = 64 << 10
	return &Client{
		endpoint: origin + "/v1/agent/configurations",
		reports:  origin + "/v1/agent/reports",
		key:      key,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

type Response struct {
	StatusCode int
	Header     http.Header
	ETag       string
	Collection *Collection // nil on 304 or any failure, never an authoritative empty set
}

func (c *Client) Fetch(ctx context.Context, committedETag string) (Response, error) {
	if committedETag != "" && !validETag(committedETag) {
		return Response{}, errors.New("invalid committed HTTP validator")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return Response{}, errors.New("cannot construct Registry request")
	}
	request.Header.Set("Authorization", "ApiKey "+c.key)
	request.Header.Set("Accept", "application/json")
	if committedETag != "" {
		request.Header.Set("If-None-Match", committedETag)
	}
	res, err := c.do(request)
	if err != nil {
		return Response{}, err
	}

	defer res.Body.Close()
	response := Response{StatusCode: res.StatusCode, Header: res.Header}
	switch res.StatusCode {
	case http.StatusNotModified:
		if committedETag == "" {
			return response, errors.New("unexpected 304 without a committed validator")
		}
		return response, nil
	case http.StatusOK:
		if len(res.Header.Values("ETag")) != 1 || !validETag(res.Header.Get("ETag")) {
			return response, errors.New("Registry response has no valid single ETag")
		}
		mediaType, params, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" ||
			params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8") {
			return response, errors.New("Registry response must be UTF-8 application/json")
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, MaxCollectionBytes+1))
		if err != nil {
			if ctx.Err() != nil {
				return response, ctx.Err()
			}
			return response, errors.New("cannot read complete Registry response")
		}
		collection, err := decodeCollection(body)
		if err != nil {
			return response, err
		}
		response.ETag = res.Header.Get("ETag")
		response.Collection = collection
		return response, nil
	default:
		return response, fmt.Errorf("Registry returned HTTP %d", res.StatusCode)
	}
}

func (c *Client) do(request *http.Request) (*http.Response, error) {
	res, err := c.http.Do(request)
	if err == nil {
		return res, nil
	}
	// Transport errors can embed URLs, redirect targets or arbitrary text.
	if request.Context().Err() != nil {
		return nil, request.Context().Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, errors.New("Registry request timed out")
	}
	return nil, errors.New("Registry transport failed (check connectivity and TLS trust)")
}

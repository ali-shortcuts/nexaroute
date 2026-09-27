package remote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultUserAgent = "nexaroute-decision/1.0"
)

// Client is minimal reusable external DecisionProvider transport
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	userAgent  string
}

// ClientConfig for construction
type ClientConfig struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client // optional override for tests
	UserAgent  string
	// AllowHTTPForTest allows http scheme for httptest servers
	AllowHTTPForTest bool
}

// NewClient creates a new remote client with secure defaults
func NewClient(cfg ClientConfig) (*Client, error) {
	if err := ValidateBaseURL(cfg.BaseURL, cfg.AllowHTTPForTest); err != nil {
		return nil, err
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://www.jevai.org"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		transport := NewTransport()
		httpClient = &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second, // overall timeout, but context timeout is authoritative
			// Disable redirects to prevent Authorization leakage
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Do not follow redirects
				return http.ErrUseLastResponse
			},
		}
	} else {
		// Ensure redirect policy is safe even for test client
		if httpClient.CheckRedirect == nil {
			httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			}
		}
	}

	ua := cfg.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}

	return &Client{
		httpClient: httpClient,
		baseURL:    baseURL,
		apiKey:     cfg.APIKey,
		userAgent:  ua,
	}, nil
}

// NewTestClient creates a client for tests with loopback allowed and custom base URL
func NewTestClient(baseURL, apiKey string, httpClient *http.Client) (*Client, error) {
	if httpClient == nil {
		transport := NewTestTransport()
		httpClient = &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return NewClient(ClientConfig{
		BaseURL:          baseURL,
		APIKey:           apiKey,
		HTTPClient:       httpClient,
		AllowHTTPForTest: true,
	})
}

// Do sends a POST request with bounded bodies, context-aware, no retry
func (c *Client) Do(ctx context.Context, path string, requestBody []byte) (responseBody []byte, statusCode int, err error) {
	if len(requestBody) > MaxRequestBodyBytes {
		return nil, 0, &Error{Kind: ErrRequestTooLarge, Message: fmt.Sprintf("request %d > %d", len(requestBody), MaxRequestBodyBytes)}
	}

	// Build URL
	url := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(requestBody))
	if err != nil {
		return nil, 0, &Error{Kind: ErrInvalidConfig, Message: "failed to create request"}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	// Do exactly one call, no retry
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Check context cancellation
		if ctx.Err() != nil {
			return nil, 0, &Error{Kind: ErrTimeout, Message: "context canceled"}
		}
		// Classify timeout
		if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline") {
			return nil, 0, &Error{Kind: ErrTimeout, Message: "timeout"}
		}
		return nil, 0, &Error{Kind: ErrUnavailable, Message: "connection error"}
	}
	defer resp.Body.Close()

	// Handle redirect as error (do not follow, do not leak auth)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, resp.StatusCode, &Error{Kind: ErrRedirectNotAllowed, StatusCode: resp.StatusCode, Message: "redirect not allowed"}
	}

	// Bound response reading
	limited := io.LimitReader(resp.Body, MaxResponseBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, resp.StatusCode, &Error{Kind: ErrInvalidResponse, Message: "failed to read response"}
	}
	if len(body) > MaxResponseBodyBytes {
		return nil, resp.StatusCode, &Error{Kind: ErrResponseTooLarge, Message: fmt.Sprintf("response %d > %d", len(body), MaxResponseBodyBytes)}
	}

	// HTTP status handling: only 2xx success
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, resp.StatusCode, NewHTTPError(resp.StatusCode)
	}

	return body, resp.StatusCode, nil
}

// BaseURL returns configured base URL (for testing/debugging, not for logging secrets)
func (c *Client) BaseURL() string {
	return c.baseURL
}

// HasAPIKey reports whether API key is configured (for admin snapshot, no secret disclosure)
func (c *Client) HasAPIKey() bool {
	return c.apiKey != ""
}

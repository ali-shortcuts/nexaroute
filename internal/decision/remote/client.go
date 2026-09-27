package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultUserAgent = "nexaroute-decision/1.0"
	// maxRedirects caps redirect chains. Production never follows redirects
	// (see redirectPolicy), so this is defense in depth.
	maxRedirects = 5
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

// validateRedirectURL applies the SSRF policy to a redirect destination:
// no userinfo, https only (http only when allowHTTPAndPrivate is set for
// tests), and no blocked hosts. It is called for EVERY redirect destination
// before the client decides what to do with it.
func validateRedirectURL(u *url.URL, allowHTTPAndPrivate bool) error {
	if u == nil {
		return fmt.Errorf("%w: nil redirect url", ErrInvalidConfig)
	}
	if u.User != nil {
		return fmt.Errorf("%w: userinfo in redirect", ErrInvalidConfig)
	}
	if u.Scheme != "https" && !(allowHTTPAndPrivate && u.Scheme == "http") {
		return fmt.Errorf("%w: redirect scheme must be https", ErrInvalidConfig)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: redirect host required", ErrInvalidConfig)
	}
	if !allowHTTPAndPrivate {
		if err := checkDialHost(u.Hostname()); err != nil {
			return err
		}
	}
	return nil
}

// redirectPolicy returns the redirect policy for the remote-decision client.
// Every redirect destination is SSRF-validated (blocked destinations such as
// 127.0.0.1, 169.254.169.254 or metadata.google.internal are rejected with
// ErrSSRFBlocked), and redirects are NEVER followed: the 3xx response is
// returned to the caller as ErrRedirectNotAllowed. Not following redirects
// also guarantees Authorization headers can never leak to another target and
// that redirect chains cannot walk toward internal services. Even if a
// redirect were followed (e.g. a custom client), the transport still
// SSRF-validates and IP-pins the connection to the destination host.
func redirectPolicy(allowHTTPAndPrivate bool) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if err := validateRedirectURL(req.URL, allowHTTPAndPrivate); err != nil {
			return err
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("%w: too many redirects", ErrRedirectNotAllowed)
		}
		return http.ErrUseLastResponse
	}
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
			// Validate every redirect destination and never follow redirects
			// (prevents SSRF via redirect and Authorization leakage).
			CheckRedirect: redirectPolicy(cfg.AllowHTTPForTest),
		}
	} else {
		// Ensure redirect policy is safe even for test client
		if httpClient.CheckRedirect == nil {
			httpClient.CheckRedirect = redirectPolicy(cfg.AllowHTTPForTest)
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
			// CheckRedirect is left nil so NewClient installs the shared
			// redirect policy (validated destinations, never followed).
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
		// SSRF rejections (dial policy or redirect policy) surface as-is.
		if errors.Is(err, ErrSSRFBlocked) {
			return nil, 0, &Error{Kind: ErrSSRFBlocked, Message: "ssrf blocked"}
		}
		// Policy rejections (bad redirect target, malformed address).
		if errors.Is(err, ErrInvalidConfig) {
			return nil, 0, &Error{Kind: ErrInvalidConfig, Message: "invalid target"}
		}
		// Check context cancellation
		if ctx.Err() != nil {
			return nil, 0, &Error{Kind: ErrTimeout, Message: "context canceled"}
		}
		// Classify timeout
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline") {
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

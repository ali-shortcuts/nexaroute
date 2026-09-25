package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

// Client is a minimal external-decision HTTP client: exactly one POST per
// call, no retries, no redirects (credentials must never hop origins), and
// bounded request/response bodies. A Client is safe for concurrent use and
// immutable after construction, so hot reload swaps it atomically.
type Client struct {
	http *http.Client
}

// NewClient returns a production client with TLS verification and redirect
// rejection. Per-request timeouts come from ctx (the orchestrator enforces
// decision.timeout_ms); no client-level timeout is set so cancellation
// semantics stay single-sourced.
func NewClient() *Client {
	return &Client{http: &http.Client{CheckRedirect: rejectRedirect}}
}

// NewClientWithTransport returns a client over a custom RoundTripper. Tests
// use it to inject httptest servers; production code must use NewClient.
func NewClientWithTransport(rt http.RoundTripper) *Client {
	return &Client{http: &http.Client{Transport: rt, CheckRedirect: rejectRedirect}}
}

func rejectRedirect(req *http.Request, via []*http.Request) error {
	return fmt.Errorf("external decision redirects are not followed")
}

// PostJSON performs exactly one authenticated JSON POST and returns the
// bounded response body. The API key travels ONLY in the Authorization
// header; it never appears in errors, and the remote body never appears in
// errors either (status classes only).
func (c *Client) PostJSON(ctx context.Context, url, apiKey string, body []byte) ([]byte, error) {
	if len(apiKey) == 0 || len(apiKey) > MaxAPIKeyBytes {
		return nil, NewError(ClassUnavailable, decision.ReasonExternalProviderUnavailable, "external decision provider unavailable")
	}
	if len(body) == 0 || len(body) > MaxRequestBytes {
		return nil, NewError(ClassRequestTooLarge, decision.ReasonExternalRequestTooLarge, "external decision request exceeds size limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, NewError(ClassUnavailable, decision.ReasonExternalProviderUnavailable, "external decision request could not be built")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	// No proxy-auth, no cookies, no client headers: only the three headers
	// above are ever sent.

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer resp.Body.Close()
	// Bounded read FIRST, so even error responses cannot blow memory; the
	// body is then classified by status only and never surfaced.
	bounded := io.LimitReader(resp.Body, MaxResponseBytes+1)
	data, err := io.ReadAll(bounded)
	if err != nil {
		return nil, NewError(ClassInvalidResponse, decision.ReasonExternalInvalidResponse, "external decision response could not be read")
	}
	if len(data) > MaxResponseBytes {
		return nil, NewError(ClassResponseTooLarge, decision.ReasonExternalResponseTooLarge, "external decision response exceeds size limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, NewHTTPError(resp.StatusCode)
	}
	return data, nil
}

func contextError(err error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return NewError(ClassTimeout, decision.ReasonExternalTimeout, "external decision timed out")
	}
	return NewError(ClassCanceled, decision.ReasonExternalTimeout, "external decision canceled")
}

func transportError(ctx context.Context, err error) *Error {
	if ctx.Err() != nil {
		return contextError(ctx.Err())
	}
	var urlErr interface{ Timeout() bool }
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return NewError(ClassTimeout, decision.ReasonExternalTimeout, "external decision timed out")
	}
	// Deliberately coarse: DNS, connect, TLS, and redirect rejections all
	// map here with a static message. No addresses, no TLS details, no URLs.
	return NewError(ClassHTTP, decision.ReasonExternalHTTPError, "external decision request failed")
}

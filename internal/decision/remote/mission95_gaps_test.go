package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// errRoundTripper returns a fixed error from RoundTrip.
type errRoundTripper struct{ err error }

func (e errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, e.err
}

// bodyErrRoundTripper returns a response whose body fails on read.
type bodyErrRoundTripper struct{}

func (bodyErrRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       io.NopCloser(errReader{}),
	}, nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, fmt.Errorf("boom read") }

func mustClient(t *testing.T, rt http.RoundTripper) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{
		BaseURL:    "http://example.test",
		APIKey:     "k",
		HTTPClient: &http.Client{Transport: rt},
		// AllowHTTPForTest must be true for the http test base URL.
		AllowHTTPForTest: true,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestMission95ValidateRedirectURLBranches(t *testing.T) {
	if err := validateRedirectURL(nil, false); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil: got %v, want ErrInvalidConfig", err)
	}
	u, _ := url.Parse("https://user:pass@example.com/x")
	if err := validateRedirectURL(u, false); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("userinfo: got %v", err)
	}
	u, _ = url.Parse("http://example.com/x")
	if err := validateRedirectURL(u, false); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("http prod: got %v", err)
	}
	if err := validateRedirectURL(u, true); err != nil {
		t.Fatalf("http allowed: %v", err)
	}
	u, _ = url.Parse("https:///x")
	if err := validateRedirectURL(u, false); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty host: got %v", err)
	}
	u, _ = url.Parse("https://127.0.0.1/x")
	if err := validateRedirectURL(u, false); !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("blocked host: got %v", err)
	}
	u, _ = url.Parse("https://example.com/x")
	if err := validateRedirectURL(u, false); err != nil {
		t.Fatalf("public https: %v", err)
	}
}

func TestMission95RedirectPolicyTooManyAndUseLast(t *testing.T) {
	pol := redirectPolicy(true)
	req, _ := http.NewRequest("GET", "http://example.com/a", nil)
	via := make([]*http.Request, 5)
	for i := range via {
		via[i] = req
	}
	if err := pol(req, via); !errors.Is(err, ErrRedirectNotAllowed) {
		t.Fatalf("too many: got %v", err)
	}
	if err := pol(req, via[:1]); err != http.ErrUseLastResponse {
		t.Fatalf("single redirect: got %v, want ErrUseLastResponse", err)
	}
	if err := pol(req, nil); err != http.ErrUseLastResponse {
		t.Fatalf("no via: got %v", err)
	}
}

func TestMission95NewClientDefaultsAndAccessors(t *testing.T) {
	c, err := NewClient(ClientConfig{})
	if err != nil {
		t.Fatalf("empty config: %v", err)
	}
	if got := c.BaseURL(); got != "https://www.jevai.org" {
		t.Fatalf("BaseURL = %q", got)
	}
	if c.HasAPIKey() {
		t.Fatalf("HasAPIKey should be false without a key")
	}
	c2, err := NewClient(ClientConfig{BaseURL: "https://example.com/", APIKey: "k", UserAgent: "test-agent"})
	if err != nil {
		t.Fatalf("custom: %v", err)
	}
	if got := c2.BaseURL(); got != "https://example.com" {
		t.Fatalf("trailing slash trimmed: %q", got)
	}
	if !c2.HasAPIKey() {
		t.Fatalf("HasAPIKey should be true with a key")
	}
	if _, err := NewClient(ClientConfig{BaseURL: "https://user:pw@example.com/"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("userinfo base: got %v", err)
	}
	// Custom client without CheckRedirect gets the shared policy installed.
	hc := &http.Client{Transport: NewTestTransport()}
	if _, err := NewClient(ClientConfig{BaseURL: "http://example.test", HTTPClient: hc, AllowHTTPForTest: true}); err != nil {
		t.Fatalf("custom client: %v", err)
	}
	if hc.CheckRedirect == nil {
		t.Fatalf("CheckRedirect should be installed for a custom client")
	}
	// Custom client with its own policy keeps it.
	sentinel := func(*http.Request, []*http.Request) error { return fmt.Errorf("sentinel") }
	hc2 := &http.Client{Transport: NewTestTransport(), CheckRedirect: sentinel}
	if _, err := NewClient(ClientConfig{BaseURL: "http://example.test", HTTPClient: hc2, AllowHTTPForTest: true}); err != nil {
		t.Fatalf("custom client 2: %v", err)
	}
	// Function values are not comparable; compare behaviour instead.
	req, _ := http.NewRequest("GET", "http://example.test/", nil)
	if err := hc2.CheckRedirect(req, nil); err == nil || err.Error() != "sentinel" {
		t.Fatalf("custom CheckRedirect not preserved: %v", err)
	}
}

func TestMission95DoErrorMappings(t *testing.T) {
	ctx := context.Background()

	c := mustClient(t, errRoundTripper{err: fmt.Errorf("wrap: %w", ErrSSRFBlocked)})
	if _, _, err := c.Do(ctx, "/x", []byte("{}")); !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("ssrf: %v", err)
	}
	c = mustClient(t, errRoundTripper{err: fmt.Errorf("wrap: %w", ErrInvalidConfig)})
	if _, _, err := c.Do(ctx, "/x", []byte("{}")); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid config: %v", err)
	}
	c = mustClient(t, errRoundTripper{err: errors.New("read: connection timeout exceeded")})
	if _, _, err := c.Do(ctx, "/x", []byte("{}")); !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout wording: %v", err)
	}
	c = mustClient(t, errRoundTripper{err: context.DeadlineExceeded})
	if _, _, err := c.Do(ctx, "/x", []byte("{}")); !errors.Is(err, ErrTimeout) {
		t.Fatalf("deadline: %v", err)
	}
	c = mustClient(t, errRoundTripper{err: errors.New("connection refused")})
	if _, _, err := c.Do(ctx, "/x", []byte("{}")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("generic: %v", err)
	}
	// Cancelled context takes precedence over the transport error.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	c = mustClient(t, errRoundTripper{err: errors.New("connection refused")})
	if _, _, err := c.Do(canceled, "/x", []byte("{}")); !errors.Is(err, ErrTimeout) {
		t.Fatalf("canceled ctx: %v", err)
	}
	// Malformed path: control character makes request construction fail.
	c = mustClient(t, errRoundTripper{err: nil})
	if _, _, err := c.Do(ctx, "/x\nbad", []byte("{}")); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("bad path: %v", err)
	}
	// Body read failure surfaces as ErrInvalidResponse.
	c = mustClient(t, bodyErrRoundTripper{})
	if _, _, err := c.Do(ctx, "/x", []byte("{}")); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("body read: %v", err)
	}
}

func TestMission95ErrorHelpers(t *testing.T) {
	if got := (&Error{Kind: ErrHTTPError, StatusCode: 503}).Error(); got != "external http error: status=503" {
		t.Fatalf("with status: %q", got)
	}
	if got := (&Error{Kind: ErrTimeout}).Error(); got != "external timeout" {
		t.Fatalf("without status: %q", got)
	}
	long := strings.Repeat("x", 300)
	if got := NewError(ErrInvalidResponse, long); len(got.Message) != 256 {
		t.Fatalf("truncation: len %d", len(got.Message))
	}
	if got := NewHTTPError(404); got.StatusCode != 404 || !errors.Is(got, ErrHTTPError) {
		t.Fatalf("NewHTTPError: %+v", got)
	}
	if !IsUnknownCandidate(NewError(ErrUnknownCandidate, "nope")) {
		t.Fatalf("IsUnknownCandidate should match")
	}
	if IsUnknownCandidate(NewError(ErrTimeout, "nope")) {
		t.Fatalf("IsUnknownCandidate should not match timeout")
	}
	if !IsTimeout(NewError(ErrTimeout, "t")) || !IsHTTPError(NewHTTPError(500)) ||
		!IsRequestTooLarge(NewError(ErrRequestTooLarge, "r")) ||
		!IsResponseTooLarge(NewError(ErrResponseTooLarge, "r")) ||
		!IsInvalidResponse(NewError(ErrInvalidResponse, "r")) {
		t.Fatalf("Is* helpers mismatch")
	}
}

func TestMission95CheckDialHostBranches(t *testing.T) {
	for _, host := range []string{"", ".", "exa%mple.com", "a..b", "bad host.com"} {
		if err := checkDialHost(host); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("checkDialHost(%q) = %v, want ErrInvalidConfig", host, err)
		}
	}
	if err := checkDialHost("8.8.8.8"); err != nil {
		t.Fatalf("public literal: %v", err)
	}
	if err := checkDialHost("example.com."); err != nil {
		t.Fatalf("trailing dot: %v", err)
	}
}

func TestMission95SSRFUnparsableAndEmptyResolution(t *testing.T) {
	// Unparsable address: IP slice of an impossible length.
	resolve := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.IP{1, 2, 3}}}, nil
	}
	d := ssrfDialContext(resolve, func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dial must not be called for unparsable addresses")
		return nil, nil
	})
	if _, err := d(context.Background(), "tcp", "example.com:443"); !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("unparsable: %v", err)
	}
	// Empty resolution fails closed with ErrUnavailable.
	resolveEmpty := func(context.Context, string) ([]net.IPAddr, error) {
		return nil, nil
	}
	d = ssrfDialContext(resolveEmpty, func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dial must not be called for empty resolution")
		return nil, nil
	})
	if _, err := d(context.Background(), "tcp", "example.com:443"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("empty: %v", err)
	}
	// Malformed dial address.
	d = ssrfDialContext(resolveEmpty, nil)
	if _, err := d(context.Background(), "tcp", "no-port-here"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("malformed addr: %v", err)
	}
	// Blocked IP literal dial path.
	d = ssrfDialContext(resolveEmpty, func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dial must not be called for blocked literals")
		return nil, nil
	})
	if _, err := d(context.Background(), "tcp", "127.0.0.1:443"); !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("blocked literal: %v", err)
	}
}

func TestMission95ValidateBaseURLBranches(t *testing.T) {
	if err := ValidateBaseURL("", false); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if err := ValidateBaseURL("https://example.com", false); err != nil {
		t.Fatalf("https: %v", err)
	}
	if err := ValidateBaseURL("ftp://example.com", false); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("ftp: %v", err)
	}
	if err := ValidateBaseURL("https://", false); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("no host: %v", err)
	}
	if err := ValidateBaseURL("https://127.0.0.1", false); !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("loopback prod: %v", err)
	}
	if err := ValidateBaseURL("http://127.0.0.1:8080", true); err != nil {
		t.Fatalf("http test: %v", err)
	}
	// Unparsable URL: control character in host.
	if err := ValidateBaseURL("https://exa\nmple.com", false); err == nil {
		t.Fatalf("control char URL should fail")
	}
}

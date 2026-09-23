package httpapi

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestClassifyTransportError(t *testing.T) {
	t.Parallel()
	dns := &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "x.example", IsNotFound: true}}
	dnsServer := &net.OpError{Op: "dial", Err: &net.DNSError{Err: "server misbehaving", Name: "y.example"}}
	timeout := &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}
	refused := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	reset := &net.OpError{Op: "read", Err: syscall.ECONNRESET}
	tlsBad := &net.OpError{Op: "remote error", Err: x509.UnknownAuthorityError{}}
	wrapped := &url.Error{Op: "Post", URL: "http://x/v1", Err: refused}

	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{context.Canceled, "caller_cancelled"},
		{context.DeadlineExceeded, "provider_timeout"},
		{fmt.Errorf("wrap: %w", timeout), "provider_timeout"},
		{dns, "dns_not_found"},
		{dnsServer, "dns_failure"},
		{refused, "connection_refused"},
		{reset, "connection_reset"},
		{tlsBad, "tls_failure"},
		{wrapped, "connection_refused"},
		{errors.New("mystery"), "provider_connection_failed"},
	}
	for _, tc := range cases {
		if got := classifyTransportError(tc.err); got != tc.want {
			t.Fatalf("classifyTransportError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

var _ = http.StatusOK
var _ = strings.Contains

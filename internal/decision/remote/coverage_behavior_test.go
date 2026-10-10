package remote

import (
	"errors"
	"strings"
	"testing"
)

func TestCoverageBehaviorTypedErrorsAreBoundedAndDiscoverable(t *testing.T) {
	long := strings.Repeat("secret-body", 100)
	err := NewError(ErrInvalidResponse, long)
	if len(err.Message) != 256 {
		t.Fatalf("message length=%d want 256", len(err.Message))
	}
	if !IsInvalidResponse(err) || errors.Is(err, ErrHTTPError) {
		t.Fatal("typed error classification is incorrect")
	}
	httpErr := NewHTTPError(429)
	if !IsHTTPError(httpErr) || httpErr.StatusCode != 429 || !strings.Contains(httpErr.Error(), "status=429") {
		t.Fatalf("http error=%+v text=%q", httpErr, httpErr.Error())
	}
	wrapped := NewError(ErrTimeout, "deadline")
	if !IsTimeout(wrapped) || wrapped.Unwrap() != ErrTimeout {
		t.Fatal("timeout unwrap/classification failed")
	}
}

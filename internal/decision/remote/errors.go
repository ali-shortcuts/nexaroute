package remote

import (
	"errors"
	"fmt"
)

// Typed error classes for external provider — bounded, secret-safe, no raw remote body
var (
	ErrRequestTooLarge    = errors.New("external request too large")
	ErrResponseTooLarge   = errors.New("external response too large")
	ErrHTTPError          = errors.New("external http error")
	ErrInvalidResponse    = errors.New("external invalid response")
	ErrUnknownCandidate   = errors.New("external unknown candidate")
	ErrTimeout            = errors.New("external timeout")
	ErrUnavailable        = errors.New("external provider unavailable")
	ErrInvalidConfig      = errors.New("external invalid config")
	ErrRedirectNotAllowed = errors.New("redirect not allowed")
	ErrSSRFBlocked        = errors.New("ssrf blocked")
)

// Error is a bounded, secret-safe wrapper
type Error struct {
	Kind       error
	StatusCode int
	Message    string // bounded, no secrets, no raw remote body
}

func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s: status=%d", e.Kind.Error(), e.StatusCode)
	}
	return e.Kind.Error()
}

func (e *Error) Unwrap() error {
	return e.Kind
}

func NewError(kind error, msg string) *Error {
	if len(msg) > 256 {
		msg = msg[:256]
	}
	return &Error{Kind: kind, Message: msg}
}

func NewHTTPError(statusCode int) *Error {
	return &Error{Kind: ErrHTTPError, StatusCode: statusCode, Message: fmt.Sprintf("http %d", statusCode)}
}

// Is functions for error classification
func IsRequestTooLarge(err error) bool {
	return errors.Is(err, ErrRequestTooLarge)
}
func IsResponseTooLarge(err error) bool {
	return errors.Is(err, ErrResponseTooLarge)
}
func IsHTTPError(err error) bool {
	return errors.Is(err, ErrHTTPError)
}
func IsInvalidResponse(err error) bool {
	return errors.Is(err, ErrInvalidResponse)
}
func IsUnknownCandidate(err error) bool {
	return errors.Is(err, ErrUnknownCandidate)
}
func IsTimeout(err error) bool {
	return errors.Is(err, ErrTimeout)
}
func IsUnavailable(err error) bool {
	return errors.Is(err, ErrUnavailable)
}

package remote

import (
	"fmt"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

// Class is the bounded external failure classification. It drives the reason
// code and the metrics outcome; it never carries payload data.
type Class string

const (
	ClassTimeout          Class = "timeout"
	ClassCanceled         Class = "canceled"
	ClassHTTP             Class = "http"
	ClassInvalidResponse  Class = "invalid_response"
	ClassUnknownCandidate Class = "unknown_candidate"
	ClassRequestTooLarge  Class = "request_too_large"
	ClassResponseTooLarge Class = "response_too_large"
	ClassUnavailable      Class = "unavailable"
)

// Error is a typed, secret-safe external provider failure. Messages are
// static templates plus a numeric status code at most: they never include
// API keys, request/response bodies, remote error text, URLs with
// credentials, or the opaque-to-physical candidate map.
type Error struct {
	class      Class
	statusCode int
	reason     decision.ReasonCode
	msg        string
}

// NewError builds a typed error with a static, secret-safe message.
func NewError(class Class, reason decision.ReasonCode, msg string) *Error {
	if !reason.Valid() {
		reason = decision.ReasonProviderError
	}
	return &Error{class: class, reason: reason, msg: msg}
}

// NewHTTPError builds a typed HTTP-status failure. Only the numeric status
// is recorded; the remote body is always discarded.
func NewHTTPError(statusCode int) *Error {
	return &Error{
		class:      ClassHTTP,
		statusCode: statusCode,
		reason:     decision.ReasonExternalHTTPError,
		msg:        fmt.Sprintf("external decision HTTP %d", statusCode),
	}
}

func (e *Error) Error() string {
	if e == nil {
		return "external decision error"
	}
	return e.msg
}

// DecisionReason implements the orchestrator's coded-error contract.
func (e *Error) DecisionReason() decision.ReasonCode {
	if e == nil || !e.reason.Valid() {
		return decision.ReasonProviderError
	}
	return e.reason
}

// Class returns the bounded failure class.
func (e *Error) Class() Class {
	if e == nil {
		return ClassUnavailable
	}
	return e.class
}

// StatusCode returns the HTTP status for ClassHTTP failures, else 0.
func (e *Error) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}

// Timeout reports whether the failure was a deadline/cancellation while
// waiting on the remote provider.
func (e *Error) Timeout() bool {
	return e != nil && (e.class == ClassTimeout || e.class == ClassCanceled)
}

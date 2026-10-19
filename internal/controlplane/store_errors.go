package controlplane

import (
	"context"
	"errors"
	"fmt"
)

type storeUnavailableError struct {
	op  string
	err error
}

func (e *storeUnavailableError) Error() string { return e.op + ": " + e.err.Error() }

func (e *storeUnavailableError) Unwrap() []error { return []error{ErrUnavailable, e.err} }

func wrapStoreError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", op, err)
	}
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) ||
		errors.Is(err, ErrMalformedSnapshot) || errors.Is(err, ErrInvalidRevision) {
		return fmt.Errorf("%s: %w", op, err)
	}
	return &storeUnavailableError{op: op, err: err}
}

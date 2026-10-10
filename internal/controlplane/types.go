package controlplane

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound        = errors.New("control-plane object not found")
	ErrConflict        = errors.New("control-plane revision conflict")
	ErrUnavailable     = errors.New("control-plane store unavailable")
	ErrInvalidRevision = errors.New("invalid control-plane revision")
)

type FailureMode string

const (
	FailClosed    FailureMode = "fail_closed"
	FailOpen      FailureMode = "fail_open"
	LastKnownGood FailureMode = "last_known_good"
)

func (m FailureMode) Valid() bool { return m == FailClosed || m == FailOpen || m == LastKnownGood }

type Snapshot struct {
	Revision  uint64    `json:"revision"`
	Schema    int       `json:"schema"`
	Payload   []byte    `json:"payload"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Store interface {
	Get(ctx context.Context, namespace string) (Snapshot, error)
	Put(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (Snapshot, error)
	Delete(ctx context.Context, namespace string, expectedRevision uint64) error
}

type Availability struct {
	Config    FailureMode `json:"config"`
	Identity  FailureMode `json:"identity"`
	Budget    FailureMode `json:"budget"`
	RateLimit FailureMode `json:"rate_limit"`
}

type ReconcileResult struct {
	Namespace string `json:"namespace"`
	Revision  uint64 `json:"revision"`
	Source    string `json:"source"` // durable | last_known_good
}

type Health struct {
	Loads       uint64 `json:"loads"`
	Saves       uint64 `json:"saves"`
	Unavailable uint64 `json:"unavailable"`
	Conflicts   uint64 `json:"conflicts"`
}

func (a Availability) Validate() error {
	if !a.Config.Valid() || !a.Identity.Valid() || !a.Budget.Valid() || !a.RateLimit.Valid() {
		return errors.New("all control-plane failure modes must be fail_open, fail_closed, or last_known_good")
	}
	return nil
}

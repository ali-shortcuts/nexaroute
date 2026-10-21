package virtualkeys

import (
	"errors"
	"fmt"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

var (
	ErrIDRequired          = errors.New("client key id is required")
	ErrMutationUnavailable = errors.New("client key mutation is unavailable")
)

type NotFoundError struct {
	ID string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("client key %q not found", e.ID)
}

type MutateFunc func(func(*config.Config) error) (config.Config, error)

type Service struct {
	mutate MutateFunc
}

func NewService(mutate MutateFunc) *Service {
	return &Service{mutate: mutate}
}

func (s *Service) Revoke(id string) error {
	if id == "" {
		return ErrIDRequired
	}
	if s == nil || s.mutate == nil {
		return ErrMutationUnavailable
	}
	_, err := s.mutate(func(cfg *config.Config) error {
		for i := range cfg.ClientAuth.VirtualKeys {
			if cfg.ClientAuth.VirtualKeys[i].ID == id {
				cfg.ClientAuth.VirtualKeys[i].Revoked = true
				return nil
			}
		}
		return &NotFoundError{ID: id}
	})
	return err
}

//go:build !linux

package secrets

import (
	"context"
	"errors"
)

type keyringProvider struct{}

func newKeyringProvider() KeyProvider { return keyringProvider{} }
func (keyringProvider) Load(context.Context, string) ([]byte, error) {
	return nil, errors.New("operating-system keyring provider is unavailable on this build")
}
func (keyringProvider) Store(context.Context, string, []byte) error {
	return errors.New("operating-system keyring provider is unavailable on this build")
}

//go:build linux

package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type linuxKeyringProvider struct{}

func newKeyringProvider() KeyProvider { return linuxKeyringProvider{} }

func (linuxKeyringProvider) Load(ctx context.Context, ref string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "secret-tool", "lookup", "service", "nexaroute", "account", ref).Output()
	if err != nil {
		return nil, fmt.Errorf("OS keyring lookup failed: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil || len(key) != 32 {
		return nil, errors.New("OS keyring contains an invalid 32-byte key")
	}
	return key, nil
}

func (linuxKeyringProvider) Store(ctx context.Context, ref string, key []byte) error {
	if len(key) != 32 {
		return errors.New("keyring requires exactly 32 bytes")
	}
	cmd := exec.CommandContext(ctx, "secret-tool", "store", "service", "nexaroute", "account", ref)
	cmd.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString(key) + "\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("OS keyring store failed: %w", err)
	} else if len(output) > 0 {
		_ = output
	}
	return nil
}

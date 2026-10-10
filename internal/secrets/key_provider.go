package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// KeyProvider supplies the 32-byte key-encryption key used by the nxs1
// envelope format. Providers fail closed on missing or malformed keys.
type KeyProvider interface {
	Load(context.Context, string) ([]byte, error)
	Store(context.Context, string, []byte) error
}

// CommandProvider delegates custody to a local helper or KMS adapter. The
// helper receives "get <ref>" or "put <ref>" on stdin; secrets never appear
// in argv or error messages.
type CommandProvider struct{ Command string }

func (p CommandProvider) Load(ctx context.Context, ref string) ([]byte, error) {
	return runProviderCommand(ctx, p.Command, "get", ref, nil)
}
func (p CommandProvider) Store(ctx context.Context, ref string, key []byte) error {
	if len(key) != 32 {
		return errors.New("key provider requires exactly 32 bytes")
	}
	_, err := runProviderCommand(ctx, p.Command, "put", ref, key)
	return err
}

func runProviderCommand(ctx context.Context, command, op, ref string, key []byte) ([]byte, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("key provider command is not configured")
	}
	input := op + " " + ref + "\n"
	if key != nil {
		input += base64.StdEncoding.EncodeToString(key) + "\n"
	}
	cmd := exec.CommandContext(ctx, command)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("key provider command failed: %w", err)
	}
	if op == "put" {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil || len(decoded) != 32 {
		return nil, errors.New("key provider returned an invalid 32-byte key")
	}
	return decoded, nil
}

// EnvProvider is useful when a secret injector exposes a base64-encoded key.
// The environment variable name is the provider reference, not the value.
type EnvProvider struct{}

func (EnvProvider) Load(_ context.Context, ref string) ([]byte, error) {
	name := strings.TrimSpace(ref)
	if name == "" || os.Getenv(name) == "" {
		return nil, errors.New("key provider environment variable is missing")
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv(name))
	if err != nil || len(key) != 32 {
		return nil, errors.New("key provider environment value must be base64-encoded 32-byte key")
	}
	return key, nil
}
func (EnvProvider) Store(context.Context, string, []byte) error {
	return errors.New("environment key provider is read-only")
}

func configuredProvider() (KeyProvider, string, bool, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NEXAROUTE_KEY_PROVIDER"))) {
	case "", "file":
		return nil, "", false, nil
	case "env":
		return EnvProvider{}, strings.TrimSpace(os.Getenv("NEXAROUTE_KEY_PROVIDER_ENV")), true, nil
	case "command", "kms":
		ref := strings.TrimSpace(os.Getenv("NEXAROUTE_KEY_PROVIDER_REF"))
		if ref == "" {
			ref = "nexaroute/master-key"
		}
		return CommandProvider{Command: os.Getenv("NEXAROUTE_KEY_PROVIDER_COMMAND")}, ref, true, nil
	case "keyring":
		return newKeyringProvider(), "nexaroute/master-key", true, nil
	default:
		return nil, "", false, fmt.Errorf("unsupported NEXAROUTE_KEY_PROVIDER")
	}
}

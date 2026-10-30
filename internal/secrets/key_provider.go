package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("key provider command failed: %w", err)
	}
	stderr := &cappedBuffer{limit: 64 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("key provider command failed: %w", err)
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, providerCommandMaxOutput+1))
	if readErr == nil && len(out) > providerCommandMaxOutput {
		// Stop reading: the helper is flooding stdout. Kill it instead
		// of waiting on a pipe nobody drains, then fail closed.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errors.New("key provider output exceeds the 1 MiB size bound")
	}
	if waitErr := cmd.Wait(); waitErr != nil {
		return nil, fmt.Errorf("key provider command failed: %w", waitErr)
	}
	if readErr != nil {
		return nil, fmt.Errorf("key provider command failed: %w", readErr)
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

// providerCommandMaxOutput bounds what runProviderCommand reads from a
// helper's stdout. The shipped Vault helper already caps its Vault
// response at 1 MiB, and vault-accept caps its Vault responses at the
// same bound; without a gateway-side bound, an arbitrary command
// provider could make the gateway buffer unbounded output before the
// 32-byte key check rejects it. The bound applies to both get and put:
// a put helper is expected to print nothing.
const providerCommandMaxOutput = 1 << 20

// cappedBuffer keeps at most limit bytes, discarding the rest, so a
// chatty helper cannot grow process memory through stderr. Stderr
// content is never included in returned errors.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len() >= b.limit {
		return len(p), nil
	}
	if n := b.limit - b.buf.Len(); n < len(p) {
		p = p[:n]
	}
	return b.buf.Write(p)
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
	case "vault-transit", "transit":
		return nil, "", false, errors.New("Vault Transit is an envelope-key provider and cannot supply a raw master key; use LoadCipher")
	default:
		return nil, "", false, fmt.Errorf("unsupported NEXAROUTE_KEY_PROVIDER")
	}
}

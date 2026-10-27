package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/secrets"
)

func runSecrets(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nexaroute secrets status|verify|rotate|decrypt|migrate-envelope|rewrap")
	}
	cmd := args[0]
	fs := flag.NewFlagSet("secrets "+cmd, flag.ContinueOnError)
	path := fs.String("config", defaultConfigPath(), "path to JSON config")
	toStdout := fs.Bool("to-stdout", false, "write decrypted config to stdout")
	allow := fs.Bool("allow-plaintext", false, "explicitly permit plaintext output")
	legacyKeyFile := fs.String("legacy-key-file", "", "path to the legacy nxs1 32-byte master key (migrate-envelope)")
	legacyKeyB64 := fs.String("legacy-key", "", "base64 legacy nxs1 32-byte master key (migrate-envelope)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("config path required")
	}
	switch cmd {
	case "status":
		b, e := os.ReadFile(*path)
		if e != nil {
			return e
		}
		count := strings.Count(string(b), secrets.Prefix) + strings.Count(string(b), secrets.EnvelopePrefix)
		fmt.Printf("encrypted_fields=%d\n", count)
		if count == 0 {
			fmt.Println("plaintext_fields_may_exist=true")
		}
		return nil
	case "verify":
		c, e := config.LoadBase(*path)
		if e != nil {
			return e
		}
		n := 0
		for _, p := range c.Providers {
			if p.APIKey != "" {
				fmt.Printf("OK provider:%s.api_key\n", p.ID)
				n++
			}
			for i, x := range p.Credentials {
				if x.APIKey != "" {
					fmt.Printf("OK provider:%s.credentials[%d].api_key\n", p.ID, i)
					n++
				}
			}
		}
		for _, p := range c.DecisionProviders {
			if p.APIKey != "" {
				fmt.Printf("OK decision_provider:%s.api_key\n", p.ID)
				n++
			}
		}
		if c.Admin.APIKey != "" {
			fmt.Println("OK admin.api_key")
			n++
		}
		for i, k := range c.ClientAuth.Keys {
			if k != "" {
				fmt.Printf("OK client_auth.keys[%d]\n", i)
				n++
			}
		}
		fmt.Printf("verified_fields=%d\n", n)
		return nil
	case "decrypt":
		if !*toStdout || !*allow {
			return errors.New("plaintext output requires both --to-stdout and --allow-plaintext")
		}
		b, e := os.ReadFile(*path)
		if e != nil {
			return e
		}
		cipher, e := secrets.LoadCipher(*path)
		if e != nil {
			return e
		}
		plain, _, e := secrets.TransformConfigWith(cipher, b, false)
		if e != nil {
			return e
		}
		_, e = os.Stdout.Write(append(plain, '\n'))
		return e
	case "rotate":
		if os.Getenv("NEXAROUTE_MASTER_KEY") != "" || os.Getenv("NEXAROUTE_MASTER_KEY_FILE") != "" || strings.TrimSpace(os.Getenv("NEXAROUTE_KEY_PROVIDER")) != "" && os.Getenv("NEXAROUTE_KEY_PROVIDER") != "file" {
			return errors.New("rotate requires the auto-managed <config>.key file; external key providers must perform rotation in their custody system")
		}
		b, e := os.ReadFile(*path)
		if e != nil {
			return e
		}
		old, _, e := secrets.Key(*path)
		if e != nil {
			return e
		}
		plain, _, e := secrets.TransformConfig(b, old, false)
		if e != nil {
			return e
		}
		next := make([]byte, 32)
		if _, e = rand.Read(next); e != nil {
			return errors.New("cannot generate replacement master key")
		}
		encrypted, _, e := secrets.TransformConfig(plain, next, true)
		if e != nil {
			return errors.New("cannot re-encrypt secrets")
		}
		kp := *path + ".key"
		oldCopy := kp + ".previous"
		nextPath := kp + ".next"
		if e = writeRotateFile(oldCopy, old); e != nil {
			return errors.New("cannot retain recovery copy of prior key")
		}
		// Persist the new key before replacing the config. If the process stops
		// after the config rename, Key() can authenticate the staged key against
		// the config and atomically promote it on the next startup.
		if e = writeRotateFile(nextPath, next); e != nil {
			return errors.New("cannot stage replacement master key")
		}
		if e = writeRotateFile(*path, append(encrypted, '\n')); e != nil {
			return e
		}
		if e = os.Rename(nextPath, kp); e != nil {
			return fmt.Errorf("config rotated but key activation is pending; restart will attempt recovery using %s: %w", filepath.Base(nextPath), e)
		}
		syncRotateDirectory(filepath.Dir(kp))
		fmt.Println("master key rotated; recovery copy retained at " + filepath.Base(oldCopy))
		return nil
	case "migrate-envelope":
		env, enabled, e := secrets.ConfiguredEnvelope()
		if e != nil {
			return e
		}
		if !enabled {
			return errors.New("migrate-envelope requires NEXAROUTE_KEY_PROVIDER=vault-transit")
		}
		legacy, e := loadLegacyKey(*legacyKeyFile, *legacyKeyB64)
		if e != nil {
			return e
		}
		n, e := secrets.MigrateToEnvelopeFile(context.Background(), *path, legacy, env)
		if e != nil {
			return e
		}
		fmt.Printf("migrated_fields=%d\n", n)
		return nil
	case "rewrap":
		env, enabled, e := secrets.ConfiguredEnvelope()
		if e != nil {
			return e
		}
		if !enabled {
			return errors.New("rewrap requires NEXAROUTE_KEY_PROVIDER=vault-transit")
		}
		n, e := secrets.RewrapConfigFile(context.Background(), *path, env)
		if e != nil {
			return e
		}
		fmt.Printf("rewrapped_fields=%d\n", n)
		return nil
	default:
		return errors.New("unknown secrets command; use status, verify, rotate, decrypt, migrate-envelope, or rewrap")
	}
}

func loadLegacyKey(file, encoded string) ([]byte, error) {
	if file == "" {
		file = strings.TrimSpace(os.Getenv("NEXAROUTE_LEGACY_MASTER_KEY_FILE"))
	}
	if encoded == "" {
		encoded = strings.TrimSpace(os.Getenv("NEXAROUTE_LEGACY_MASTER_KEY"))
	}
	if file != "" && encoded != "" {
		return nil, errors.New("set only one legacy nxs1 key source")
	}
	if encoded != "" {
		b, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(b) != 32 {
			return nil, errors.New("legacy nxs1 key must be base64 of exactly 32 bytes")
		}
		return b, nil
	}
	if file == "" {
		return nil, errors.New("migrate-envelope requires --legacy-key-file or --legacy-key for nxs1 envelopes")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, errors.New("cannot read legacy nxs1 key file")
	}
	st, err := os.Stat(file)
	if err != nil || st.Mode().Perm() != 0600 || len(b) != 32 {
		return nil, errors.New("legacy nxs1 key file must contain exactly 32 bytes with mode 0600")
	}
	return b, nil
}
func writeRotateFile(path string, b []byte) error {
	d := filepath.Dir(path)
	f, e := os.CreateTemp(d, ".nexaroute-secret-*")
	if e != nil {
		return e
	}
	n := f.Name()
	defer func() { _ = os.Remove(n) }()
	if e = f.Chmod(0600); e != nil {
		_ = f.Close()
		return e
	}
	if _, e = f.Write(b); e != nil {
		_ = f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		_ = f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(n, path); e != nil {
		return e
	}
	syncRotateDirectory(d)
	return nil
}

func syncRotateDirectory(path string) {
	if x, err := os.Open(path); err == nil {
		_ = x.Sync()
		_ = x.Close()
	}
}

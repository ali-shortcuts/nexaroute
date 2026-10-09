package main

import (
	"crypto/rand"
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
		return errors.New("usage: nexaroute secrets status|verify|rotate|decrypt")
	}
	cmd := args[0]
	fs := flag.NewFlagSet("secrets "+cmd, flag.ContinueOnError)
	path := fs.String("config", defaultConfigPath(), "path to JSON config")
	toStdout := fs.Bool("to-stdout", false, "write decrypted config to stdout")
	allow := fs.Bool("allow-plaintext", false, "explicitly permit plaintext output")
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
		count := strings.Count(string(b), secrets.Prefix)
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
		k, _, e := secrets.Key(*path)
		if e != nil {
			return e
		}
		plain, _, e := secrets.TransformConfig(b, k, false)
		if e != nil {
			return e
		}
		_, e = os.Stdout.Write(append(plain, '\n'))
		return e
	case "rotate":
		if os.Getenv("NEXAROUTE_MASTER_KEY") != "" || os.Getenv("NEXAROUTE_MASTER_KEY_FILE") != "" {
			return errors.New("rotate currently requires the auto-managed <config>.key file; clear master-key environment overrides after safely arranging key custody")
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
		if e = os.WriteFile(oldCopy, old, 0600); e != nil {
			return errors.New("cannot retain recovery copy of prior key")
		}
		if e = writeRotateFile(*path, append(encrypted, '\n')); e != nil {
			return e
		}
		if e = writeRotateFile(kp, next); e != nil {
			return fmt.Errorf("config rotated but key activation failed; restore %s: %w", oldCopy, e)
		}
		fmt.Println("master key rotated; recovery copy retained at " + filepath.Base(oldCopy))
		return nil
	default:
		return errors.New("unknown secrets command; use status, verify, rotate, or decrypt")
	}
}
func writeRotateFile(path string, b []byte) error {
	d := filepath.Dir(path)
	f, e := os.CreateTemp(d, ".nexaroute-secret-*")
	if e != nil {
		return e
	}
	n := f.Name()
	defer os.Remove(n)
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(n, path); e != nil {
		return e
	}
	if x, e := os.Open(d); e == nil {
		_ = x.Sync()
		_ = x.Close()
	}
	return nil
}

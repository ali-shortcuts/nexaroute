package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const Prefix = "nxs1:"

var cryptoRead = rand.Read

// Key obtains the configured master key or creates the per-config default key.
// NEXAROUTE_MASTER_KEY is a base64-encoded 32-byte key; otherwise
// NEXAROUTE_MASTER_KEY_FILE may select a key file, or <config>.key is used.
func Key(configPath string) ([]byte, string, error) {
	if v := os.Getenv("NEXAROUTE_MASTER_KEY"); v != "" {
		b, e := base64.StdEncoding.DecodeString(v)
		if e != nil || len(b) != 32 {
			return nil, "", errors.New("invalid NEXAROUTE_MASTER_KEY: set a base64-encoded 32-byte key")
		}
		return b, keyID(b), nil
	}
	p := os.Getenv("NEXAROUTE_MASTER_KEY_FILE")
	if p == "" {
		p = configPath + ".key"
	}
	b, e := os.ReadFile(p)
	if e == nil {
		st, se := os.Stat(p)
		if se != nil {
			return nil, "", errors.New("cannot inspect master key file")
		}
		if st.Mode().Perm() != 0600 {
			return nil, "", fmt.Errorf("master key file permissions must be 0600; fix with chmod 600 %s", p)
		}
		if len(b) != 32 {
			return nil, "", fmt.Errorf("master key file must contain exactly 32 raw bytes: %s", p)
		}
		return b, keyID(b), nil
	}
	if !os.IsNotExist(e) {
		return nil, "", errors.New("cannot read master key file; check path and permissions")
	}
	if os.Getenv("NEXAROUTE_MASTER_KEY_FILE") != "" {
		return nil, "", fmt.Errorf("master key file not found; create a 32-byte key at %s or set NEXAROUTE_MASTER_KEY", p)
	}
	if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return nil, "", errors.New("cannot create config directory for master key")
	}
	b = make([]byte, 32)
	if _, e = cryptoRead(b); e != nil {
		return nil, "", errors.New("cannot generate master key")
	}
	f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return nil, "", errors.New("cannot create master key file; set NEXAROUTE_MASTER_KEY or NEXAROUTE_MASTER_KEY_FILE")
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		_ = os.Remove(p)
		return nil, "", errors.New("cannot persist master key")
	}
	if d, x := os.Open(filepath.Dir(p)); x == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return b, keyID(b), nil
}
func keyID(k []byte) string { s := sha256.Sum256(k); return hex.EncodeToString(s[:8]) }
func seal(key []byte, aad, plain []byte) (string, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return "", e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	dek := make([]byte, 32)
	if _, e = cryptoRead(dek); e != nil {
		return "", e
	}
	db, _ := aes.NewCipher(dek)
	dg, _ := cipher.NewGCM(db)
	nonce := make([]byte, dg.NonceSize())
	if _, e = cryptoRead(nonce); e != nil {
		return "", e
	}
	dc := dg.Seal(nil, nonce, plain, aad)
	wn := make([]byte, g.NonceSize())
	if _, e = cryptoRead(wn); e != nil {
		return "", e
	}
	wrapped := g.Seal(nil, wn, dek, []byte(keyID(key)))
	payload := append(append([]byte{}, wn...), wrapped...)
	payload = append(payload, dc...)
	return Prefix + keyID(key) + ":" + base64.RawStdEncoding.EncodeToString(nonce) + ":" + base64.RawStdEncoding.EncodeToString(payload), nil
}
func Open(key []byte, aad []byte, text string) ([]byte, error) {
	parts := strings.Split(text, ":")
	if len(parts) != 4 || parts[0] != "nxs1" || parts[1] != keyID(key) {
		return nil, errors.New("invalid ciphertext or master key; restore the correct key")
	}
	decode := func(s string) ([]byte, error) {
		b, err := base64.RawStdEncoding.DecodeString(s)
		if err != nil || base64.RawStdEncoding.EncodeToString(b) != s {
			return nil, errors.New("invalid ciphertext encoding")
		}
		return b, nil
	}
	nonce, e := decode(parts[2])
	if e != nil {
		return nil, errors.New("invalid ciphertext encoding")
	}
	p, e := decode(parts[3])
	if e != nil {
		return nil, errors.New("invalid ciphertext encoding")
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, errors.New("invalid master key")
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	if len(nonce) != 12 || len(p) < g.NonceSize()+g.Overhead()+16 {
		return nil, errors.New("invalid ciphertext length")
	}
	wn := p[:g.NonceSize()]
	wrapped := p[g.NonceSize() : g.NonceSize()+32+g.Overhead()]
	dek, e := g.Open(nil, wn, wrapped, []byte(parts[1]))
	if e != nil {
		return nil, errors.New("ciphertext authentication failed; restore the correct master key")
	}
	dc := p[g.NonceSize()+32+g.Overhead():]
	db, _ := aes.NewCipher(dek)
	dg, _ := cipher.NewGCM(db)
	return dg.Open(nil, nonce, dc, aad)
}
func AAD(provider, field string) []byte { return []byte(provider + "\x00" + field) }

// TransformConfig changes secret-bearing JSON values in-place. The caller must
// provide a fully decrypted document before runtime use.
func TransformConfig(raw []byte, key []byte, encrypt bool) ([]byte, int, error) {
	var root map[string]any
	if e := json.Unmarshal(raw, &root); e != nil {
		return nil, 0, e
	}
	n := 0
	visit := func(owner, field string, v any) (any, error) {
		s, ok := v.(string)
		if !ok || s == "" {
			return v, nil
		}
		if encrypt {
			if strings.HasPrefix(s, Prefix) {
				return s, nil
			}
			x, e := seal(key, AAD(owner, field), []byte(s))
			if e != nil {
				return nil, e
			}
			n++
			return x, nil
		}
		if !strings.HasPrefix(s, Prefix) {
			return s, nil
		}
		x, e := Open(key, AAD(owner, field), s)
		if e != nil {
			return nil, e
		}
		n++
		return string(x), nil
	}
	if ps, ok := root["providers"].([]any); ok {
		for _, x := range ps {
			p, ok := x.(map[string]any)
			if !ok {
				continue
			}
			id, _ := p["id"].(string)
			for _, f := range []string{"api_key", "proxy_url"} {
				if v, ok := p[f]; ok {
					z, e := visit(id, f, v)
					if e != nil {
						return nil, n, fmt.Errorf("provider %s field %s: %w", id, f, e)
					}
					p[f] = z
				}
			}
			if hs, ok := p["headers"].(map[string]any); ok {
				for k, v := range hs {
					z, e := visit(id, "header:"+k, v)
					if e != nil {
						return nil, n, fmt.Errorf("provider %s header: %w", id, e)
					}
					hs[k] = z
				}
			}
			if cs, ok := p["credentials"].([]any); ok {
				for i, c := range cs {
					m, ok := c.(map[string]any)
					if !ok {
						continue
					}
					f := fmt.Sprintf("credentials[%d].api_key", i)
					z, e := visit(id, f, m["api_key"])
					if e != nil {
						return nil, n, fmt.Errorf("provider %s credential field: %w", id, e)
					}
					if _, ok := m["api_key"]; ok {
						m["api_key"] = z
					}
				}
			}
		}
	}
	if ds, ok := root["decision_providers"].([]any); ok {
		for _, x := range ds {
			p, ok := x.(map[string]any)
			if ok {
				id, _ := p["id"].(string)
				z, e := visit(id, "api_key", p["api_key"])
				if e != nil {
					return nil, n, fmt.Errorf("decision provider %s api_key: %w", id, e)
				}
				if _, ok := p["api_key"]; ok {
					p["api_key"] = z
				}
			}
		}
	}
	if adm, ok := root["admin"].(map[string]any); ok {
		z, e := visit("admin", "api_key", adm["api_key"])
		if e != nil {
			return nil, n, fmt.Errorf("admin api_key: %w", e)
		}
		if _, ok := adm["api_key"]; ok {
			adm["api_key"] = z
		}
	}
	if ca, ok := root["client_auth"].(map[string]any); ok {
		if ks, ok := ca["keys"].([]any); ok {
			for i, v := range ks {
				z, e := visit("client_auth", fmt.Sprintf("keys[%d]", i), v)
				if e != nil {
					return nil, n, fmt.Errorf("client_auth keys[%d]: %w", i, e)
				}
				ks[i] = z
			}
		}
	}
	b, e := json.MarshalIndent(root, "", "  ")
	return b, n, e
}

// EncryptedBackup wraps original config bytes in an authenticated encrypted blob.
func EncryptedBackup(key, raw []byte) ([]byte, error) {
	v, e := seal(key, []byte("nexaroute-backup-v1"), raw)
	if e != nil {
		return nil, e
	}
	return []byte(v + "\n"), nil
}
func DecryptBackup(key []byte, raw []byte) ([]byte, error) {
	return Open(key, []byte("nexaroute-backup-v1"), strings.TrimSpace(string(raw)))
}
func BackupName(path string) string {
	return path + "." + time.Now().UTC().Format("20060102T150405.000000000Z") + ".enc.bak"
}

package secrets

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestEnvelopeRoundTripAndTampering(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	aad := AAD("provider-a", "api_key")
	got, e := seal(key, aad, []byte("roundtrip-secret"))
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(got, "nxs1:") {
		t.Fatal("missing version")
	}
	plain, e := Open(key, aad, got)
	if e != nil || string(plain) != "roundtrip-secret" {
		t.Fatalf("round trip: %q %v", plain, e)
	}
	for i := range got {
		b := []byte(got)
		b[i] ^= 1
		if _, e := Open(key, aad, string(b)); e == nil {
			t.Fatalf("tampering byte %d authenticated", i)
		}
	}
	if _, e = Open(key, AAD("provider-b", "api_key"), got); e == nil {
		t.Fatal("AAD provider move authenticated")
	}
	if _, e = Open(key, AAD("provider-a", "other"), got); e == nil {
		t.Fatal("AAD field move authenticated")
	}
}
func TestTransformConfigFields(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	raw := []byte(`{"providers":[{"id":"p","api_key":"key","credentials":[{"api_key":"pool"}],"headers":{"X-Key":"header"}}],"decision_providers":[{"id":"d","api_key":"decision"}],"admin":{"api_key":"admin"},"client_auth":{"keys":["client"]}}`)
	enc, n, e := TransformConfig(raw, key, true)
	if e != nil || n != 6 {
		t.Fatalf("encrypt n=%d err=%v", n, e)
	}
	if strings.Contains(string(enc), `"api_key":"pool"`) || strings.Contains(string(enc), `"keys":["client"]`) || !strings.Contains(string(enc), Prefix) {
		t.Fatal("plaintext persisted")
	}
	enc2, n, e := TransformConfig(enc, key, true)
	if e != nil || n != 0 || string(enc2) != string(enc) {
		t.Fatalf("not idempotent n=%d err=%v", n, e)
	}
	dec, n, e := TransformConfig(enc, key, false)
	if e != nil || n != 6 || !strings.Contains(string(dec), "pool") || !strings.Contains(string(dec), "client") {
		t.Fatalf("decrypt n=%d err=%v", n, e)
	}
}
func TestEncryptedBackup(t *testing.T) {
	k := []byte("01234567890123456789012345678901")
	in := []byte(`{"api_key":"must-not-appear"}`)
	b, e := EncryptedBackup(k, in)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "must-not-appear") {
		t.Fatal("backup plaintext")
	}
	out, e := DecryptBackup(k, b)
	if e != nil || string(out) != string(in) {
		t.Fatalf("backup restore: %s %v", out, e)
	}
}
func TestKeySourcesAndPermissions(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	k, _, e := Key(filepath.Join(t.TempDir(), "config"))
	if e != nil || len(k) != 32 {
		t.Fatalf("env key: %v", e)
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", "broken")
	if _, _, e = Key("x"); e == nil {
		t.Fatal("accepted bad env key")
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "master")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", path)
	if e = os.WriteFile(path, make([]byte, 32), 0600); e != nil {
		t.Fatal(e)
	}
	k, _, e = Key("unused")
	if e != nil {
		t.Fatal(e)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if _, _, e = Key("unused"); e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(path, 0644); e != nil {
		t.Fatal(e)
	}
	if _, _, e = Key("unused"); e == nil {
		t.Fatal("accepted loose key file")
	}
}
func FuzzOpenNeverPanics(f *testing.F) {
	f.Add("nxs1:bad::")
	f.Add("nxs1:0000000000000000:AAAA:AAAA")
	f.Fuzz(func(t *testing.T, s string) { _, _ = Open(make([]byte, 32), []byte("x"), s) })
}
func TestKeyFileAutoCreateAndMissingExplicit(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	k, _, e := Key(path)
	if e != nil || len(k) != 32 {
		t.Fatalf("create: %v", e)
	}
	st, e := os.Stat(path + ".key")
	if e != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("created key mode=%v err=%v", st, e)
	}
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, _, e = Key(path); e == nil {
		t.Fatal("missing explicit key accepted")
	}
}
func TestConcurrentEnvelopeRoundTrips(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := []byte(strings.Repeat("x", i+1))
			c, e := seal(key, AAD("p", "api_key"), msg)
			if e != nil {
				errCh <- e
				return
			}
			got, e := Open(key, AAD("p", "api_key"), c)
			if e != nil {
				errCh <- e
				return
			}
			if string(got) != string(msg) {
				errCh <- errors.New("round trip mismatch")
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}
}
func TestErrorPathsAndBackupName(t *testing.T) {
	k := []byte("01234567890123456789012345678901")
	if _, e := seal([]byte("short"), nil, nil); e == nil {
		t.Fatal("invalid sealing key accepted")
	}
	for _, s := range []string{"", "nxs1:a:b:c", "nxs1:" + keyID(k) + ":%%%:AA", "nxs1:" + keyID(k) + ":" + base64.RawStdEncoding.EncodeToString(make([]byte, 12)) + ":" + base64.RawStdEncoding.EncodeToString(make([]byte, 100))} {
		if _, e := Open(k, nil, s); e == nil {
			t.Fatalf("malformed ciphertext accepted: %q", s)
		}
	}
	if _, _, e := TransformConfig([]byte("{"), k, false); e == nil {
		t.Fatal("invalid JSON accepted")
	}
	if _, e := EncryptedBackup([]byte("short"), nil); e == nil {
		t.Fatal("invalid backup key accepted")
	}
	if _, e := DecryptBackup(k, []byte("not ciphertext")); e == nil {
		t.Fatal("bad backup accepted")
	}
	a := BackupName("config")
	if !strings.HasPrefix(a, "config.") || !strings.HasSuffix(a, ".enc.bak") {
		t.Fatal("bad backup name")
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	dir := t.TempDir()
	notDir := filepath.Join(dir, "file")
	if e := os.WriteFile(notDir, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e := Key(filepath.Join(notDir, "config")); e == nil {
		t.Fatal("key directory creation failure ignored")
	}
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", notDir)
	if _, _, e := Key("unused"); e == nil {
		t.Fatal("key path that is not a key file accepted")
	}
}

func TestKeyLengthAndAADFieldFailures(t *testing.T) {
	t.Setenv("NEXAROUTE_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 31)))
	if _, _, e := Key("config"); e == nil {
		t.Fatal("accepted short environment key")
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	p := filepath.Join(t.TempDir(), "badkey")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", p)
	if e := os.WriteFile(p, []byte("short"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e := Key("unused"); e == nil {
		t.Fatal("accepted short key file")
	}
	dirPath := filepath.Join(t.TempDir(), "directory-key")
	if e := os.Mkdir(dirPath, 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", dirPath)
	if _, _, e := Key("unused"); e == nil {
		t.Fatal("accepted directory as key file")
	}
	k := []byte("01234567890123456789012345678901")
	for _, tc := range []struct {
		doc string
		aad []byte
	}{{`{"providers":[{"id":"other","api_key":"PLACE"}]}`, AAD("original", "api_key")}, {`{"decision_providers":[{"id":"other","api_key":"PLACE"}]}`, AAD("original", "api_key")}, {`{"admin":{"api_key":"PLACE"}}`, AAD("wrong", "api_key")}, {`{"client_auth":{"keys":["PLACE"]}}`, AAD("client_auth", "keys[1]")}} {
		ciphertext, e := seal(k, tc.aad, []byte("x"))
		if e != nil {
			t.Fatal(e)
		}
		doc := strings.Replace(tc.doc, "PLACE", ciphertext, 1)
		if _, _, e = TransformConfig([]byte(doc), k, false); e == nil {
			t.Fatalf("AAD mismatch accepted: %s", doc)
		}
	}
}

func TestRandomSourceFailures(t *testing.T) {
	old := cryptoRead
	defer func() { cryptoRead = old }()
	key := []byte("01234567890123456789012345678901")
	for failAt := 1; failAt <= 3; failAt++ {
		calls := 0
		cryptoRead = func(b []byte) (int, error) {
			calls++
			if calls == failAt {
				return 0, errors.New("injected random failure")
			}
			return len(b), nil
		}
		if _, e := seal(key, nil, []byte("x")); e == nil {
			t.Fatalf("seal random call %d failure ignored", failAt)
		}
	}
	t.Setenv("NEXAROUTE_MASTER_KEY", "")
	t.Setenv("NEXAROUTE_MASTER_KEY_FILE", "")
	cryptoRead = func([]byte) (int, error) { return 0, errors.New("injected random failure") }
	if _, _, e := Key(filepath.Join(t.TempDir(), "config")); e == nil {
		t.Fatal("key-generation random failure ignored")
	}
}

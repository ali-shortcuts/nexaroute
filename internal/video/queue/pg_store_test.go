package queue

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Fail-closed behavior of the PostgreSQL job store that can be proven
// without a live database: bad configuration is a hard error, never a
// silent fallback, and the DSN never leaks into errors.
func TestOpenPostgresStoreRejectsEmptyDSN(t *testing.T) {
	_, err := OpenPostgresStore(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "DSN is required") {
		t.Fatalf("empty DSN should fail, got %v", err)
	}
}

func TestOpenPostgresStoreFailsClosedOnUnreachable(t *testing.T) {
	dsn := "postgres://nexaroute:secret@127.0.0.1:1/nexaroute_test?sslmode=disable"
	start := time.Now()
	_, err := OpenPostgresStore(context.Background(), dsn)
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("unreachable postgres should fail closed, got %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("DSN leaked into error: %v", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("unreachable open took too long: %v", time.Since(start))
	}
}

func TestOpenPostgresStoreRejectsGarbageDSN(t *testing.T) {
	_, err := OpenPostgresStore(context.Background(), "://not a dsn")
	if err == nil {
		t.Fatal("garbage DSN should fail")
	}
	if strings.Contains(err.Error(), "not a dsn") {
		t.Fatalf("DSN leaked into error: %v", err)
	}
}

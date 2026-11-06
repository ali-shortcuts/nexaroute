package queue

import (
	"context"
	"testing"
)

// Agent C coverage: the Postgres job store open paths.

// A malformed DSN is only parsed lazily by the pgx driver, so it surfaces
// at Ping time as "postgres is unreachable", not as an Open error. The
// unreachable branch is already covered by the existing read tests.
func TestOpenPostgresStoreDBWithFakeDriver(t *testing.T) {
	// Healthy fake: ping and migration succeed, so Open returns a store.
	s, err := openPostgresStoreDB(context.Background(), openFakeQueueDB(t, &fakeQueueDB{}))
	if err != nil {
		t.Fatalf("open with fake DB: %v", err)
	}
	if s == nil {
		t.Fatal("open with fake DB returned a nil store")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Migration failure (begin fails): Open must return the error and
	// close the database instead of leaking it.
	if _, err := openPostgresStoreDB(context.Background(), openFakeQueueDB(t, &fakeQueueDB{failBegin: true})); err == nil {
		t.Fatal("migration failure must make Open return an error")
	}
}

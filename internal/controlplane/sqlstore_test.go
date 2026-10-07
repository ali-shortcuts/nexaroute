package controlplane

import (
	"strings"
	"testing"
)

func TestSQLStoreRequiresDatabase(t *testing.T) {
	if _, err := NewSQLStore(nil); err == nil {
		t.Fatal("nil database accepted")
	}
}

func TestControlPlaneSQLSchemaIsPostgresSafeAndBounded(t *testing.T) {
	schema := ControlPlaneSQLSchema()
	for _, needle := range []string{"nexaroute_control_snapshots", "namespace TEXT PRIMARY KEY", "revision BIGINT NOT NULL", "payload BYTEA NOT NULL", "updated_at TIMESTAMPTZ NOT NULL"} {
		if !strings.Contains(schema, needle) {
			t.Fatalf("schema missing %q: %s", needle, schema)
		}
	}
}

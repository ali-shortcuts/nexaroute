package nexaflow

import (
	"strings"
	"testing"
)

func TestEnvSecretResolver(t *testing.T) {
	t.Setenv("NEXAFLOW_TEST_SECRET", "resolved-value")
	t.Setenv("NEXAFLOW_TEST_EMPTY", "")

	tests := []struct {
		name    string
		ref     string
		want    string
		wantErr string
	}{
		{name: "resolves set variable", ref: "NEXAFLOW_TEST_SECRET", want: "resolved-value"},
		{name: "trims surrounding whitespace", ref: "  NEXAFLOW_TEST_SECRET  ", want: "resolved-value"},
		{name: "empty reference fails closed", ref: "", wantErr: "secret reference is empty"},
		{name: "whitespace reference fails closed", ref: "   ", wantErr: "secret reference is empty"},
		{name: "unset variable fails closed", ref: "NEXAFLOW_TEST_UNSET", wantErr: "is not set"},
		{name: "empty value fails closed", ref: "NEXAFLOW_TEST_EMPTY", wantErr: "is not set"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EnvSecretResolver(tc.ref)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				if got != "" {
					t.Fatalf("value = %q on error, want empty", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("value = %q, want %q", got, tc.want)
			}
		})
	}
}

package config

import "testing"

func TestValidateSecurityStoreBackendSelection(t *testing.T) {
	base := Default()
	if err := base.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
	cases := []struct {
		name    string
		backend string
		url     string
		wantErr bool
	}{
		{"default empty", "", "", false},
		{"explicit bbolt", "bbolt", "", false},
		{"bbolt with url", "bbolt", "redis://127.0.0.1:6379", true},
		{"redis without url", "redis", "", true},
		{"redis with url", "redis", "redis://127.0.0.1:6379", false},
		{"postgres without url", "postgres", "", true},
		{"postgres with url", "postgres", "postgres://127.0.0.1:5432/nexaroute", false},
		{"unknown backend", "sqlite", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Admin.SecurityStoreBackend = tc.backend
			cfg.Admin.SecurityStoreURL = tc.url
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("backend=%q url set=%v: want validation error", tc.backend, tc.url != "")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("backend=%q: unexpected error %v", tc.backend, err)
			}
		})
	}
}

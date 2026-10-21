package virtualkeys

import (
	"errors"
	"reflect"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func cloneKeys(in []config.VirtualKeyConfig) []config.VirtualKeyConfig {
	out := append([]config.VirtualKeyConfig(nil), in...)
	for i := range out {
		out[i].AllowedModels = append([]string(nil), in[i].AllowedModels...)
		out[i].AllowedRoutes = append([]string(nil), in[i].AllowedRoutes...)
		out[i].BudgetAlertThresholds = append([]float64(nil), in[i].BudgetAlertThresholds...)
	}
	return out
}

func testMutator(keys []config.VirtualKeyConfig) (*config.Config, MutateFunc) {
	store := config.Default()
	store.ClientAuth.VirtualKeys = cloneKeys(keys)
	mutate := func(fn func(*config.Config) error) (config.Config, error) {
		candidate := store
		candidate.ClientAuth.VirtualKeys = cloneKeys(store.ClientAuth.VirtualKeys)
		if err := fn(&candidate); err != nil {
			return config.Config{}, err
		}
		store = candidate
		return store, nil
	}
	return &store, mutate
}

func TestRevokeMarksOnlyTargetAndPreservesMetadata(t *testing.T) {
	target := config.VirtualKeyConfig{
		ID: "vk_target", Name: "ci", KeyHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TenantID: "acme", ProjectID: "platform", TeamID: "engineering", Role: "developer",
		AllowedModels: []string{"team-model"}, AllowedRoutes: []string{"/v1/chat/completions"},
		ExpiresAt: "2030-01-01T00:00:00Z", RPM: 10, TPM: 20, RequirePrivacy: "no_training",
		BudgetUSD: 12.5, BudgetWebhookURL: "https://alerts.example.test/", BudgetAlertThresholds: []float64{50, 80},
		BudgetPeriod: "monthly",
	}
	other := config.VirtualKeyConfig{ID: "vk_other", KeyHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Role: "viewer"}
	store, mutate := testMutator([]config.VirtualKeyConfig{target, other})
	svc := NewService(mutate)

	if err := svc.Revoke(target.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if len(store.ClientAuth.VirtualKeys) != 2 {
		t.Fatalf("revoke changed key count: %d", len(store.ClientAuth.VirtualKeys))
	}
	got := store.ClientAuth.VirtualKeys[0]
	if !got.Revoked {
		t.Fatal("target key was not revoked")
	}
	want := target
	want.Revoked = true
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("revoke changed key metadata:\n got %+v\nwant %+v", got, want)
	}
	if store.ClientAuth.VirtualKeys[1].Revoked {
		t.Fatal("revoke touched an unrelated key")
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	key := config.VirtualKeyConfig{ID: "vk_once", KeyHash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
	store, mutate := testMutator([]config.VirtualKeyConfig{key})
	svc := NewService(mutate)

	for i := 0; i < 2; i++ {
		if err := svc.Revoke(key.ID); err != nil {
			t.Fatalf("revoke attempt %d: %v", i+1, err)
		}
	}
	if len(store.ClientAuth.VirtualKeys) != 1 {
		t.Fatalf("idempotent revoke changed key count: %d", len(store.ClientAuth.VirtualKeys))
	}
	if !store.ClientAuth.VirtualKeys[0].Revoked {
		t.Fatal("idempotent revoke left key active")
	}
}

func TestRevokeUnknownKeyFailsClosed(t *testing.T) {
	key := config.VirtualKeyConfig{ID: "vk_known", KeyHash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}
	store, mutate := testMutator([]config.VirtualKeyConfig{key})
	svc := NewService(mutate)

	err := svc.Revoke("vk_missing")
	var notFound *NotFoundError
	if !errors.As(err, &notFound) || notFound.ID != "vk_missing" {
		t.Fatalf("unknown key error = %#v, want NotFoundError for vk_missing", err)
	}
	if got := err.Error(); got != `client key "vk_missing" not found` {
		t.Fatalf("unknown key error text = %q", got)
	}
	if store.ClientAuth.VirtualKeys[0].Revoked {
		t.Fatal("unknown key revoke changed an existing key")
	}
}

func TestRevokeRequiresID(t *testing.T) {
	called := false
	svc := NewService(func(func(*config.Config) error) (config.Config, error) {
		called = true
		return config.Config{}, nil
	})
	if err := svc.Revoke(""); !errors.Is(err, ErrIDRequired) {
		t.Fatalf("empty id error = %v, want ErrIDRequired", err)
	}
	if called {
		t.Fatal("empty id reached the mutation callback")
	}
}

func TestRevokePropagatesPersistenceFailure(t *testing.T) {
	sentinel := errors.New("config write failed")
	svc := NewService(func(func(*config.Config) error) (config.Config, error) {
		return config.Config{}, sentinel
	})
	if err := svc.Revoke("vk1"); !errors.Is(err, sentinel) {
		t.Fatalf("persistence error = %v, want %v", err, sentinel)
	}
}

func TestRevokeUnavailableWithoutMutator(t *testing.T) {
	if err := NewService(nil).Revoke("vk1"); !errors.Is(err, ErrMutationUnavailable) {
		t.Fatalf("nil mutator error = %v, want ErrMutationUnavailable", err)
	}
	var svc *Service
	if err := svc.Revoke("vk1"); !errors.Is(err, ErrMutationUnavailable) {
		t.Fatalf("nil service error = %v, want ErrMutationUnavailable", err)
	}
}

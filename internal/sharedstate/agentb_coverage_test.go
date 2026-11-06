package sharedstate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
)

// Agent B coverage: the breaker document codec and the revision-guarded
// publish protocol — malformed/oversized documents, missing documents,
// cancelled contexts, encode failures, store errors, and expired local
// markers. Each test pins a fail-closed behavior: a bad document is
// never read as empty state, and a failed publish never clears a peer
// marker.

func TestAgentBMergeSkipsEmptyDeployment(t *testing.T) {
	now := time.Now().UTC()
	current := map[string]Entry{
		"":    {Status: StatusCooldown, CooldownUntil: now.Add(time.Minute)},
		"dep": {Status: StatusCooldown, CooldownUntil: now.Add(time.Minute)},
	}
	got := Merge(current, nil, now, time.Minute)
	if _, ok := got[""]; ok {
		t.Fatalf("Merge kept an empty deployment: %v", got)
	}
	if len(got) != 1 || got["dep"].Status != StatusCooldown {
		t.Fatalf("Merge = %v, want only dep", got)
	}
}

func TestAgentBEncodeDocumentNilAndLimits(t *testing.T) {
	payload, err := encodeDocument(nil, 0, 0)
	if err != nil {
		t.Fatalf("encodeDocument(nil) err = %v", err)
	}
	var doc document
	if err := json.Unmarshal(payload, &doc); err != nil || doc.Schema != BreakerSchemaVersion || len(doc.Entries) != 0 {
		t.Fatalf("encodeDocument(nil) = %s err=%v", payload, err)
	}
	entries := map[string]Entry{
		"a": {Status: StatusRetired},
		"b": {Status: StatusRetired},
	}
	if _, err := encodeDocument(entries, 1, 0); err == nil {
		t.Fatal("encodeDocument over maxEntries succeeded, want a limit error")
	}
	if _, err := encodeDocument(entries, 0, 1); err == nil {
		t.Fatal("encodeDocument over maxBytes succeeded, want a limit error")
	}
	// A cooldown deadline outside the JSON time range must surface as an
	// encode error, never as a silently dropped marker.
	farFuture := map[string]Entry{
		"a": {Status: StatusCooldown, CooldownUntil: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	if _, err := encodeDocument(farFuture, 0, 0); err == nil {
		t.Fatal("encodeDocument with an unmarshalable deadline succeeded, want an error")
	}
}

func TestAgentBDecodeDocumentEntryLimitAndEmptyKey(t *testing.T) {
	payload, err := json.Marshal(document{Schema: BreakerSchemaVersion, Entries: map[string]Entry{
		"a": {Status: StatusRetired},
		"b": {Status: StatusRetired},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeDocument(payload, 1, 0); err == nil {
		t.Fatal("decodeDocument over maxEntries succeeded, want a limit error")
	}
	raw := []byte(`{"schema":1,"entries":{"":{"status":"retired"},"dep":{"status":"retired"}}}`)
	got, err := decodeDocument(raw, 0, 0)
	if err != nil {
		t.Fatalf("decodeDocument err = %v", err)
	}
	if _, ok := got[""]; ok {
		t.Fatalf("decodeDocument kept an empty deployment: %v", got)
	}
	if len(got) != 1 || got["dep"].Status != StatusRetired {
		t.Fatalf("decodeDocument = %v, want only dep", got)
	}
}

func TestAgentBCloneEntriesNil(t *testing.T) {
	got := cloneEntries(nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("cloneEntries(nil) = %v, want a non-nil empty map", got)
	}
	got["dep"] = Entry{Status: StatusRetired}
	if len(cloneEntries(nil)) != 0 {
		t.Fatal("cloneEntries(nil) result must be independent per call")
	}
}

func agentbBreakerStore(t *testing.T, store Store, opts Options) *BreakerStore {
	t.Helper()
	bs, err := NewBreakerStore(store, opts)
	if err != nil {
		t.Fatal(err)
	}
	return bs
}

func TestAgentBLoadMissingDocumentIsEmpty(t *testing.T) {
	adapter, err := NewControlPlaneStore(scriptedControlPlane{loadErr: controlplane.ErrNotFound})
	if err != nil {
		t.Fatal(err)
	}
	bs := agentbBreakerStore(t, adapter, Options{ReplicaID: "agentb"})
	view, err := bs.Load(context.Background())
	if err != nil {
		t.Fatalf("Load missing err = %v, want nil", err)
	}
	if len(view.Entries) != 0 || view.Revision != 0 {
		t.Fatalf("Load missing = %+v, want an empty view", view)
	}
}

func TestAgentBPublishCanceledContext(t *testing.T) {
	adapter, err := NewControlPlaneStore(controlplane.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	bs := agentbBreakerStore(t, adapter, Options{ReplicaID: "agentb"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = bs.Publish(ctx, map[string]Entry{
		"dep": {Status: StatusRetired},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish canceled err = %v, want context.Canceled", err)
	}
}

func TestAgentBPublishEncodeLimitError(t *testing.T) {
	adapter, err := NewControlPlaneStore(controlplane.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	bs := agentbBreakerStore(t, adapter, Options{ReplicaID: "agentb", MaxEntries: 1})
	_, err = bs.Publish(context.Background(), map[string]Entry{
		"a": {Status: StatusRetired},
		"b": {Status: StatusRetired},
	})
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("Publish over-limit err = %v, want ErrMalformed", err)
	}
}

func TestAgentBPublishStoreErrorIsNotConflict(t *testing.T) {
	boom := errors.New("boom")
	adapter, err := NewControlPlaneStore(scriptedControlPlane{casErr: boom})
	if err != nil {
		t.Fatal(err)
	}
	bs := agentbBreakerStore(t, adapter, Options{ReplicaID: "agentb"})
	_, err = bs.Publish(context.Background(), map[string]Entry{
		"dep": {Status: StatusRetired},
	})
	if err == nil || !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrConflict) {
		t.Fatalf("Publish store error = %v, want ErrUnavailable (not ErrConflict)", err)
	}
}

func TestAgentBPublishSkipsExpiredLocalMarker(t *testing.T) {
	mem := controlplane.NewMemoryStore()
	adapter, err := NewControlPlaneStore(mem)
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingStore{Store: adapter}
	bs := agentbBreakerStore(t, counting, Options{ReplicaID: "agentb"})
	now := time.Now().UTC()
	view, err := bs.Publish(context.Background(), map[string]Entry{
		"dep": {Status: StatusCooldown, CooldownUntil: now.Add(-time.Minute)},
	})
	if err != nil {
		t.Fatalf("Publish expired err = %v", err)
	}
	if len(view.Entries) != 0 {
		t.Fatalf("Publish expired = %v, want no markers", view.Entries)
	}
	if counting.cas.Load() != 0 {
		t.Fatalf("Publish expired issued %d CompareAndSwap calls, want 0 (no-write)", counting.cas.Load())
	}
}

func TestAgentBMapControlPlaneErrorNil(t *testing.T) {
	if err := mapControlPlaneError(nil); err != nil {
		t.Fatalf("mapControlPlaneError(nil) = %v, want nil", err)
	}
}

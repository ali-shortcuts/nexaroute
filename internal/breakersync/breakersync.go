// Package breakersync wires the shared circuit-breaker state slice
// (internal/sharedstate) into a running gateway: it periodically
// publishes this replica's local cooldown/retired markers to the shared
// control-plane store and applies markers learned from peer replicas to
// the local health manager.
//
// The syncer is strictly opt-in (control_plane.breaker_sync) and runs off
// the request path. Its failure posture is fail-local: a store outage or
// a malformed shared document leaves local breaker state untouched — an
// outage never clears a local circuit and never admits traffic the local
// manager has excluded. Standalone deployments (sync disabled) are
// byte-for-byte the previous behavior.
//
// Application rules are conservative, mirroring ADR 0002/0003:
//   - remote cooldowns are applied via Manager.ApplySharedCooldown,
//     which can only extend a local cooldown, never shorten one, and
//     never touches failure accounting;
//   - a remote retired marker is applied only as a bounded cooldown of
//     MaxCooldown, never as a permanent local retirement — retirement
//     is sticky and has no un-retire path, so letting a peer impose it
//     permanently would be an irreversible action driven by remote
//     state. While the origin keeps publishing the marker, each sync
//     round re-extends the bounded cooldown;
//   - local recovery never clears a peer's marker: publication is the
//     monotonic merge implemented by sharedstate, and cooldowns end by
//     expiry only.
package breakersync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/sharedstate"
)

// DefaultInterval is the sync cadence when the operator does not
// configure one. Breaker state changes at incident cadence, not request
// cadence, so ten seconds keeps cross-replica convergence fast without
// meaningful store traffic.
const DefaultInterval = 10 * time.Second

// Manager is the slice of health.Manager the syncer needs. The concrete
// *health.Manager satisfies it.
type Manager interface {
	Snapshot() []health.State
	ApplySharedCooldown(id string, until time.Time, reason string) bool
}

// Store is the slice of sharedstate.BreakerStore the syncer needs.
type Store interface {
	Load(ctx context.Context) (sharedstate.View, error)
	Publish(ctx context.Context, local map[string]sharedstate.Entry) (sharedstate.View, error)
}

// Syncer owns one replica's publish/apply loop.
type Syncer struct {
	store       Store
	manager     Manager
	replicaID   string
	maxCooldown time.Duration
	interval    time.Duration
	logf        func(format string, args ...any)
	now         func() time.Time
}

// Options configures a Syncer. ReplicaID and Store and Manager are
// required; everything else has a conservative default.
type Options struct {
	ReplicaID   string
	MaxCooldown time.Duration // defaults to sharedstate.DefaultMaxCooldown
	Interval    time.Duration // defaults to DefaultInterval
	Logf        func(format string, args ...any)
	Now         func() time.Time
}

// New builds a Syncer over a sharedstate BreakerStore (or any Store).
func New(store Store, manager Manager, options Options) (*Syncer, error) {
	if store == nil {
		return nil, errors.New("breakersync: store is required")
	}
	if manager == nil {
		return nil, errors.New("breakersync: manager is required")
	}
	if options.ReplicaID == "" {
		return nil, errors.New("breakersync: replica ID is required")
	}
	maxCooldown := options.MaxCooldown
	if maxCooldown <= 0 {
		maxCooldown = sharedstate.DefaultMaxCooldown
	}
	interval := options.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Syncer{
		store:       store,
		manager:     manager,
		replicaID:   options.ReplicaID,
		maxCooldown: maxCooldown,
		interval:    interval,
		logf:        options.Logf,
		now:         now,
	}, nil
}

// LocalMarkers extracts the publishable markers (active cooldowns and
// retirements) from a health snapshot. Healthy, degraded, half-open and
// unknown states stay local: they flap at request cadence and the shared
// document carries only bounded or terminal markers.
func LocalMarkers(states []health.State, now time.Time) map[string]sharedstate.Entry {
	out := make(map[string]sharedstate.Entry)
	for _, s := range states {
		if s.Deployment == "" {
			continue
		}
		switch s.Status {
		case health.Retired:
			out[s.Deployment] = sharedstate.Entry{
				Status:      sharedstate.StatusRetired,
				ReasonClass: s.LastErrorClass,
				ObservedAt:  s.LastChecked,
			}
		case health.Cooldown:
			if s.CooldownUntil.After(now) {
				out[s.Deployment] = sharedstate.Entry{
					Status:        sharedstate.StatusCooldown,
					CooldownUntil: s.CooldownUntil,
					ReasonClass:   s.LastErrorClass,
					ObservedAt:    s.LastChecked,
				}
			}
		}
	}
	return out
}

// SyncOnce runs one load → apply → publish round. A load failure aborts
// the round before anything is applied or published, so local state is
// exactly as it was. A publish failure is reported but the applied
// remote markers stay: they came from a successful, validated load.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	view, err := s.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("breakersync: load shared breaker state: %w", err)
	}
	s.apply(view.Entries)
	local := LocalMarkers(s.manager.Snapshot(), s.now().UTC())
	if _, err := s.store.Publish(ctx, local); err != nil {
		return fmt.Errorf("breakersync: publish local breaker markers: %w", err)
	}
	return nil
}

func (s *Syncer) apply(entries map[string]sharedstate.Entry) {
	now := s.now().UTC()
	for deployment, entry := range entries {
		if entry.OriginReplica == s.replicaID {
			continue // our own markers need no application
		}
		switch entry.Status {
		case sharedstate.StatusCooldown:
			reason := "shared cooldown"
			if entry.ReasonClass != "" {
				reason += " (" + entry.ReasonClass + ")"
			}
			s.manager.ApplySharedCooldown(deployment, entry.CooldownUntil, reason)
		case sharedstate.StatusRetired:
			// Bounded demotion only; see the package comment. The local
			// manager's own Retire path is never invoked from remote
			// state.
			reason := "shared retired (bounded)"
			if entry.ReasonClass != "" {
				reason += " (" + entry.ReasonClass + ")"
			}
			s.manager.ApplySharedCooldown(deployment, now.Add(s.maxCooldown), reason)
		}
	}
}

// Run syncs immediately, then on the configured interval, until ctx is
// cancelled. Errors are logged and never stop the loop: the next round
// retries against the same local state.
func (s *Syncer) Run(ctx context.Context) {
	s.round(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.round(ctx)
		}
	}
}

func (s *Syncer) round(ctx context.Context) {
	if err := s.SyncOnce(ctx); err != nil && s.logf != nil {
		s.logf("breaker_sync_error=%v", err)
	}
}

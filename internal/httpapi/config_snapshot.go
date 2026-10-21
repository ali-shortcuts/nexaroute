package httpapi

import (
	"net/netip"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/pricing"
)

// configSnapshot is an immutable, atomically-published view of the effective
// runtime config (audit #371 finding C1).
//
// The cfg value — and everything reachable from it — is read-only after
// publication: handlers must never mutate it. A config reload builds a fresh
// snapshot and publishes it with a single pointer swap, so in-flight requests
// keep a coherent view of the config they started with while new requests
// observe the new config. This is the same publish-by-swap discipline the
// router tables already use.
//
// The snapshot also carries the price book built once per config revision, so
// the admission path no longer rebuilds pricing.FromConfig per request.
type configSnapshot struct {
	cfg  config.Config
	book *pricing.Book
	// trustedProxies is parsed once per config revision. Config validation
	// guarantees these entries are valid; a parse failure here degrades to
	// "trust nobody" rather than trusting a caller-controlled header.
	trustedProxies []netip.Prefix
}

// newConfigSnapshot builds the immutable snapshot for cfg. The caller must
// not mutate cfg (or anything reachable from it) after publication.
func newConfigSnapshot(cfg config.Config) *configSnapshot {
	proxies, _ := config.ParseTrustedProxyCIDRs(cfg.TLS.TrustedProxyCIDRs)
	return &configSnapshot{cfg: cfg, book: pricing.FromConfig(cfg), trustedProxies: proxies}
}

// configSnapshot returns the current immutable config snapshot. The returned
// snapshot's cfg must be treated as read-only: mutating it would corrupt the
// view of every in-flight request holding the same snapshot.
//
// Servers constructed without New (some unit tests use &Server{}) have no
// published snapshot yet; one is built and published on demand so the
// accessor is total.
func (s *Server) configSnapshot() *configSnapshot {
	if snap := s.cfgSnap.Load(); snap != nil {
		return snap
	}
	s.runtimeMu.RLock()
	cfg := s.cfg
	s.runtimeMu.RUnlock()
	snap := newConfigSnapshot(cfg)
	if s.cfgSnap.CompareAndSwap(nil, snap) {
		return snap
	}
	return s.cfgSnap.Load()
}

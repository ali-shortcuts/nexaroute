package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Client API keys (opt-in)
//
// Cloudflare AI Gateway, Kong and BricksLLM all gate the data plane with
// per-client keys and per-client rate ceilings. NexaRoute keeps local
// single-user deployments frictionless by default, but operators exposing the
// gateway beyond loopback can enable static key authentication with an
// optional per-key request-per-minute ceiling without deploying a reverse
// proxy just for auth.
//
// Comparison is constant-time over SHA-256 digests so timing cannot reveal a
// configured key. Buckets are bounded and idle-pruned like the admin buckets.

const (
	clientBucketIdle = 10 * time.Minute
	maxClientBuckets = 4096
)

type clientBucket struct {
	tokens float64
	last   time.Time
}

type clientAuthState struct {
	mu      sync.Mutex
	buckets map[string]*clientBucket
}

func (s *Server) clientAuthConfig() (bool, []string, int) {
	cfg := s.currentConfig()
	return cfg.ClientAuth.Enabled, cfg.ClientAuth.Keys, cfg.ClientAuth.RPM
}

func extractClientKey(r *http.Request) string {
	if h := r.Header.Get("x-api-key"); h != "" {
		return strings.TrimSpace(h)
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[len("bearer "):])
	}
	return auth
}

func keyDigest(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// clientKeyMatches reports whether the presented key matches any configured
// key in constant time relative to the configured key count (which is public
// configuration, not secret material).
func clientKeyMatches(keys []string, presented string) bool {
	digest := keyDigest(presented)
	matched := 0
	for _, k := range keys {
		want := keyDigest(k)
		if subtle.ConstantTimeCompare([]byte(want), []byte(digest)) == 1 {
			matched++
		}
	}
	return matched > 0
}

// clientRPMAllow enforces the per-key requests-per-minute ceiling with a
// token bucket. RPM 0 means unlimited.
func (s *Server) clientRPMAllow(presented string, rpm int) bool {
	if rpm <= 0 {
		return true
	}
	id := keyDigest(presented)
	now := time.Now()
	s.clientRL.Lock()
	defer s.clientRL.Unlock()
	if s.clientBuckets == nil {
		s.clientBuckets = map[string]*clientBucket{}
	}
	if len(s.clientBuckets) >= maxClientBuckets {
		for k, b := range s.clientBuckets {
			if now.Sub(b.last) > clientBucketIdle {
				delete(s.clientBuckets, k)
			}
		}
	}
	capacity := float64(rpm)
	refill := float64(rpm) / 60.0
	b, ok := s.clientBuckets[id]
	if !ok && len(s.clientBuckets) >= maxClientBuckets {
		return false
	}
	if !ok || now.Sub(b.last) > clientBucketIdle {
		b = &clientBucket{tokens: capacity, last: now}
		s.clientBuckets[id] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * refill
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clientAuthAllowed gates one data-plane request. It writes the protocol
// shaped error itself and returns false when the request must stop.
func (s *Server) clientAuthAllowed(w http.ResponseWriter, r *http.Request, anthropicStyle bool) bool {
	enabled, keys, rpm := s.clientAuthConfig()
	if !enabled {
		return true
	}
	key := extractClientKey(r)
	if key == "" || !clientKeyMatches(keys, key) {
		if anthropicStyle {
			anthropicErrorJSON(w, http.StatusUnauthorized, "invalid or missing API key")
		} else {
			errorJSON(w, http.StatusUnauthorized, "invalid or missing API key")
		}
		return false
	}
	if !s.clientRPMAllow(key, rpm) {
		w.Header().Set("Retry-After", "60")
		if anthropicStyle {
			anthropicErrorJSON(w, http.StatusTooManyRequests, "client rate limit exceeded; retry after 60 seconds")
		} else {
			errorJSON(w, http.StatusTooManyRequests, "client rate limit exceeded; retry after 60 seconds")
		}
		return false
	}
	return true
}

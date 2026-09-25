// Package remote provides minimal, reusable transport primitives for external
// DecisionProviders: context-aware HTTP with bounded bodies, no retries, no
// redirects, and secret-safe errors. It is deliberately small — not an SDK
// framework.
package remote

const (
	// MaxRequestBytes is the Jev-documented request body cap (32 KiB).
	// Requests are measured after marshaling; anything larger fails open
	// without sending a single byte.
	MaxRequestBytes = 32 * 1024
	// MaxResponseBytes bounds remote response reads (64 KiB). Oversized
	// responses are rejected and fail open.
	MaxResponseBytes = 64 * 1024
	// MaxAPIKeyBytes bounds credential material handled by the client.
	MaxAPIKeyBytes = 4096
	// UserAgent is the fixed client identifier for external decision calls.
	UserAgent = "NexaRoute-Decision/1.0"
)

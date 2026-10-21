package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Model discovery is a best-effort inventory helper shared by the Admin API
// and the operator acceptance command. It is deliberately bounded: one
// adapter timeout, at most two candidate paths, a capped response body and a
// capped surfaced error. Discovery is never a prerequisite for saving a
// manually configured model.
const (
	DefaultDiscoveryTimeout = 8 * time.Second
	maxDiscoveryBodyBytes   = 4 << 20
	maxDiscoveryErrorBytes  = 2048
)

// SupportsDiscovery reports whether a provider type exposes a real model
// inventory endpoint. Azure OpenAI deployments, Vertex named projects and
// Bedrock's Converse runtime do not expose a plain /models resource on their
// base URL, so the catalog/summary must not claim discovery for them even
// though ApplyDefaults fills a generic ModelsPath for legacy compatibility.
func SupportsDiscovery(p config.ProviderConfig) bool {
	switch p.Type {
	case "bedrock", "azure_openai", "vertex":
		return false
	}
	return strings.TrimSpace(p.ModelsPath) != ""
}

// DiscoverModels lists upstream model ids using the provider's configured
// ModelsPath. The caller's context is honored for cancellation; the adapter
// timeout bounds each request. An unsupported provider type returns a
// descriptive error without making a network call.
func DiscoverModels(ctx context.Context, p config.ProviderConfig) ([]string, int, error) {
	return DiscoverModelsWithTimeout(ctx, p, DefaultDiscoveryTimeout)
}

// DiscoverModelsWithTimeout is DiscoverModels with an explicit per-request
// timeout. Non-positive values fall back to DefaultDiscoveryTimeout.
func DiscoverModelsWithTimeout(ctx context.Context, p config.ProviderConfig, timeout time.Duration) ([]string, int, error) {
	p.ApplyDefaults()
	if !SupportsDiscovery(p) {
		return nil, 0, fmt.Errorf("model discovery is not supported for provider type %q", p.Type)
	}
	if timeout <= 0 {
		timeout = DefaultDiscoveryTimeout
	}
	a, err := NewAdapter(p, timeout)
	if err != nil {
		return nil, 0, err
	}
	defer CloseIdleConnections(a)
	paths := []string{p.ModelsPath}
	if p.ModelsPath == "/v1/models" {
		paths = append(paths, "/models")
	}
	var lastErr error
	lastStatus := 0
	for _, path := range uniquePaths(paths) {
		resp, err := a.DoPath(ctx, http.MethodGet, path, nil, false, nil)
		if err != nil {
			lastErr = err
			continue
		}
		lastStatus = resp.StatusCode
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBodyBytes+1))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("model discovery read: %w", readErr)
			continue
		}
		if len(b) > maxDiscoveryBodyBytes {
			lastErr = fmt.Errorf("model discovery response exceeds %d bytes", maxDiscoveryBodyBytes)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b = a.RedactBody(b)
			if len(b) > maxDiscoveryErrorBytes {
				b = append(append([]byte(nil), b[:maxDiscoveryErrorBytes]...), []byte("…")...)
			}
			lastErr = fmt.Errorf("model discovery HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			continue
		}
		models := ParseModelList(b)
		if len(models) == 0 {
			lastErr = fmt.Errorf("model endpoint returned no model ids")
			continue
		}
		return models, resp.StatusCode, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("model discovery failed")
	}
	return nil, lastStatus, lastErr
}

// ParseModelList accepts the common inventory shapes (bare array, data,
// models, items) and returns deduplicated model ids. It never invents ids.
func ParseModelList(b []byte) []string {
	const maxModels = 10000
	const maxModelIDBytes = 1024
	var root any
	if json.Unmarshal(b, &root) != nil {
		return nil
	}
	out := []string{}
	add := func(v any) {
		if len(out) >= maxModels {
			return
		}
		switch x := v.(type) {
		case string:
			x = strings.TrimSpace(x)
			x = strings.TrimPrefix(x, "models/")
			if len(x) <= maxModelIDBytes {
				out = append(out, x)
			}
		case map[string]any:
			for _, key := range []string{"id", "model", "name"} {
				if id, _ := x[key].(string); strings.TrimSpace(id) != "" {
					id = strings.TrimPrefix(id, "models/")
					if len(id) <= maxModelIDBytes {
						out = append(out, id)
					}
					return
				}
			}
		}
	}
	switch obj := root.(type) {
	case []any:
		for _, item := range obj {
			add(item)
		}
	case map[string]any:
		for _, key := range []string{"data", "models", "items"} {
			if arr, ok := obj[key].([]any); ok {
				for _, item := range arr {
					add(item)
				}
			}
		}
	}
	return uniqueModels(out)
}

func uniqueModels(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func uniquePaths(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

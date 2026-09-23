package translate

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// NameMap is a bidirectional mapping between original client-facing tool names
// and sanitized upstream tool names. OpenAI accepts ^[a-zA-Z0-9_-]{1,64}$ and
// Anthropic accepts ^[a-zA-Z0-9_-]{1,128}$, while real ecosystems (MCP servers,
// OpenRouter providers) routinely produce names with dots, colons, slashes, or
// excessive length. Renaming only when strictly necessary keeps well-formed
// names byte-identical; the reverse map restores original names in every
// response path (non-streaming and streaming content_block_start events).
type NameMap struct {
	fwd map[string]string
	rev map[string]string
}

// NewOpenAINameMap sanitizes tool names for OpenAI Chat Completions upstreams.
func NewOpenAINameMap(names []string) *NameMap {
	return newNameMap(names, 64)
}

// NewAnthropicNameMap sanitizes tool names for Anthropic Messages upstreams.
func NewAnthropicNameMap(names []string) *NameMap {
	return newNameMap(names, 128)
}

func newNameMap(names []string, maxLen int) *NameMap {
	m := &NameMap{fwd: map[string]string{}, rev: map[string]string{}}
	used := map[string]struct{}{}
	for _, original := range names {
		original = strings.TrimSpace(original)
		if original == "" {
			continue
		}
		if len(original) <= maxLen && isSafeToolName(original) {
			used[original] = struct{}{}
			continue
		}
		sanitized := sanitizeToolName(original, maxLen)
		base := sanitized
		for i := 2; ; i++ {
			if _, taken := used[sanitized]; !taken {
				break
			}
			sanitized = truncateWithSuffix(base, maxLen, fmt.Sprintf("-%d", i))
		}
		used[sanitized] = struct{}{}
		// Only map when the sanitized name actually differs; otherwise the
		// original name round-trips untouched.
		if sanitized != original {
			m.fwd[original] = sanitized
			m.rev[sanitized] = original
		}
	}
	if len(m.fwd) == 0 {
		return nil
	}
	return m
}

// Forward maps an original client name to the upstream-safe name.
func (m *NameMap) Forward(name string) string {
	if m == nil {
		return name
	}
	if mapped, ok := m.fwd[name]; ok {
		return mapped
	}
	return name
}

// Reverse maps an upstream name back to the original client-facing name.
func (m *NameMap) Reverse(name string) string {
	if m == nil {
		return name
	}
	if mapped, ok := m.rev[name]; ok {
		return mapped
	}
	return name
}

// isSafeToolName reports whether the name already satisfies both protocol
// grammars (OpenAI is the stricter of the two with its 64-char limit applied
// by the caller when maxLen==64).
func isSafeToolName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return name != ""
}

func sanitizeToolName(name string, maxLen int) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "tool"
	}
	return truncateWithSuffix(out, maxLen, "")
}

// truncateWithSuffix shortens s to maxLen. With an empty suffix it appends a
// hash-derived disambiguator when truncation occurs so long names stay unique
// instead of colliding. With a non-empty suffix (dedupe case) it makes room
// for the suffix before appending it.
func truncateWithSuffix(s string, maxLen int, suffix string) string {
	if maxLen <= 0 {
		return s
	}
	if suffix == "" {
		if len(s) <= maxLen {
			return s
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(s))
		tail := fmt.Sprintf("-%x", h.Sum64()&0xffffffff)
		keep := maxLen - len(tail)
		if keep < 1 {
			return s[:maxLen]
		}
		return s[:keep] + tail
	}
	if len(s)+len(suffix) > maxLen {
		s = s[:maxLen-len(suffix)]
	}
	return s + suffix
}

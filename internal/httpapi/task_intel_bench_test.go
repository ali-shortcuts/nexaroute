package httpapi

import (
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/taskintel"
)

// BenchmarkTaskIntel_ClassifyResolve measures the per-request cost added by
// task intelligence (feature extraction + deterministic classification +
// mode resolution) on a representative coding-shaped request. Reported in the
// PR as the measured latency budget; no LLM call, no I/O on the hot path.
func BenchmarkTaskIntel_ClassifyResolve(b *testing.B) {
	body := taskIntelBody(b, "Explain what this Go function does:\n```go\npackage main\n\nfunc greet(name string) string {\n\treturn \"hello \" + name\n}\n```")
	raw := []byte(body)
	profile := map[string]string{"coding": "code-pool", "long_context": "ctx-pool"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ti := extractFeaturesAndClassify(raw, feature.ExtractOptions{
			Protocol:      feature.ProtocolOpenAI,
			Model:         "task-model",
			VisionType:    "image_url",
			AudioType:     "input_audio",
			ContentFields: []string{"messages"},
			UserAgent:     "claude-code/1.0.0",
		})
		_ = taskintel.Resolve(taskintel.Input{
			Mode:      taskintel.ModeApply,
			Profile:   ti.Profile,
			TaskPools: profile,
		})
	}
}

// BenchmarkTaskIntel_ClassifyResolve_LongContext measures the same path on a
// large (~11k token) request, the worst realistic shape for the extractor.
func BenchmarkTaskIntel_ClassifyResolve_LongContext(b *testing.B) {
	body := taskIntelBody(b, strings.Repeat("lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor ", 600))
	raw := []byte(body)
	profile := map[string]string{"coding": "code-pool", "long_context": "ctx-pool"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ti := extractFeaturesAndClassify(raw, feature.ExtractOptions{
			Protocol:      feature.ProtocolOpenAI,
			Model:         "task-model",
			ContentFields: []string{"messages"},
		})
		_ = taskintel.Resolve(taskintel.Input{
			Mode:      taskintel.ModeApply,
			Profile:   ti.Profile,
			TaskPools: profile,
		})
	}
}

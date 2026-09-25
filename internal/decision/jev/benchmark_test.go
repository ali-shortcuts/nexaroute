package jev

import (
	"strconv"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

// Local-work benchmarks only: mapping, serialization, parsing.

func benchFeatures() decision.RequestFeatures {
	return decision.RequestFeatures{
		Tools: true, Reasoning: true, Streaming: true,
		EstimatedContextTokens: 18400, EstimatedInputTokens: 16000, MaxOutputTokens: 2400,
	}
}

func benchJevCandidates(n int) []decision.Candidate {
	out := make([]decision.Candidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, decision.Candidate{
			ID: "provider-" + strconv.Itoa(i) + "/model", PoolOrdinal: 0, Priority: 0,
			ContextWindow: 200000, Tools: true, Reasoning: true, Streaming: true,
			EWMALatencyMS: 120, EWMAFailureRate: 0.01, Observations: 100,
			HasCost: true, InputCostPerMTok: 3, OutputCostPerMTok: 15,
		})
	}
	return out
}

func BenchmarkTaskSummary(b *testing.B) {
	f := benchFeatures()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = TaskSummary(f)
	}
}

func BenchmarkBuildMapping2(b *testing.B) {
	c := benchJevCandidates(2)
	f := benchFeatures()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = BuildMapping(c, f)
	}
}

func BenchmarkBuildMapping10(b *testing.B) {
	c := benchJevCandidates(10)
	f := benchFeatures()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = BuildMapping(c, f)
	}
}

func BenchmarkBuildMapping100(b *testing.B) {
	c := benchJevCandidates(100)
	f := benchFeatures()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = BuildMapping(c, f)
	}
}

func BenchmarkParseResponse(b *testing.B) {
	body := []byte(`{"code":0,"message":"ok","data":{"decision":"c1","confidence":0.86,"probabilities":{"c0":0.14,"c1":0.86},"guidance":"ignore me"}}`)
	allowed := map[string]string{"c0": "p1/a", "c1": "p2/b"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = ParseResponse(body, allowed)
	}
}

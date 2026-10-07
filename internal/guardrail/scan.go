package guardrail

import "regexp"

type Finding struct {
	Kind     string `json:"kind"`
	Redacted bool   `json:"redacted"`
}

var patterns = map[string]*regexp.Regexp{
	"pii_email":        regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`),
	"pii_phone":        regexp.MustCompile(`\b(?:\+?\d[\d .()-]{8,}\d)\b`),
	"secret_api_key":   regexp.MustCompile(`(?i)\b(?:sk-[A-Za-z0-9_-]{16,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9_]{20,})\b`),
	"prompt_injection": regexp.MustCompile(`(?i)(ignore\s+(all|any|the)\s+previous\s+instructions|reveal\s+the\s+system\s+prompt|jailbreak\s+mode)`),
}

func Scan(body []byte) []Finding {
	out := make([]Finding, 0, 4)
	for kind, re := range patterns {
		if re.Match(body) {
			out = append(out, Finding{Kind: kind})
		}
	}
	return out
}

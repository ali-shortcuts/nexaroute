package guardrail

import (
	"regexp"
	"strings"
)

// Finding classifies one matched risk class. It never carries the
// matched content — classification only.
type Finding struct {
	Kind     string `json:"kind"`
	Redacted bool   `json:"redacted"`
}

// detector is one risk class: a candidate pattern plus an
// optional validator a candidate must pass (Luhn for cards, SSA
// rules for SSNs) to keep precision high.
type detector struct {
	kind     string
	re       *regexp.Regexp
	validate func(candidate string) bool
}

// detectors run in a FIXED order so Scan output is deterministic.
var detectors = []detector{
	{kind: "pii_email", re: regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)},
	{kind: "pii_phone", re: regexp.MustCompile(`\b(?:\+?\d[\d .()-]{8,}\d)\b`)},
	{kind: "pii_credit_card", re: regexp.MustCompile(`\b\d{4}(?:[ -]\d{4}){3}\b|\b\d{13,19}\b`), validate: validCardNumber},
	{kind: "pii_ssn", re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), validate: validSSN},
	{kind: "secret_api_key", re: regexp.MustCompile(`(?i)(?:\bsk-[A-Za-z0-9_\-]{16,}|\bAKIA[0-9A-Z]{16}|\bgh[pousr]_[A-Za-z0-9_]{20,}|\b` + "github_pat_" + `[A-Za-z0-9_]{20,}|\bxox[baprs]-[A-Za-z0-9\-]{10,}|\bAIza[0-9A-Za-z_\-]{35}|\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}|-----BEGIN (?:RSA |EC |OPENSSH |PGP )?PRIVATE KEY-----|\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis|amqp)://[^\s:/@]+:[^\s/@]{3,}@|\bBearer\s+[A-Za-z0-9._\-]{20,})`)},
	{kind: "prompt_injection", re: regexp.MustCompile(`(?i)(ignore\s+(all|any|the)\s+previous\s+instructions|disregard\s+(all\s+)?(previous|prior)\s+instructions|reveal\s+(the\s+|your\s+)?system\s+prompt|print\s+(your|the)\s+(system\s+)?(prompt|instructions)|jailbreak\s+mode|developer\s+mode|override\s+(your\s+)?(safety|instructions)|do\s+anything\s+now)`)},
}

// validCardNumber reports whether the candidate's digits (spaces
// and dashes stripped) form a 13–19 digit Luhn-valid number.
func validCardNumber(candidate string) bool {
	digits := strings.NewReplacer(" ", "", "-", "").Replace(candidate)
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if d < '0' || d > '9' {
			return false
		}
		n := int(d - '0')
		if double {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

// validSSN applies the SSA's structural rules: area 001–665 or
// 667–899 (not 000, 666, or 9xx), group 01–99, serial 0001–9999.
func validSSN(candidate string) bool {
	parts := strings.Split(candidate, "-")
	if len(parts) != 3 {
		return false
	}
	atoi := func(s string) int {
		n := 0
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return -1
			}
			n = n*10 + int(s[i]-'0')
		}
		return n
	}
	area, group, serial := atoi(parts[0]), atoi(parts[1]), atoi(parts[2])
	if area <= 0 || area == 666 || area >= 900 || group <= 0 || serial <= 0 {
		return false
	}
	return true
}

// Scan classifies a request body into risk classes. At most one
// finding per class, in the fixed detector order.
func Scan(body []byte) []Finding {
	out := make([]Finding, 0, len(detectors))
	for _, d := range detectors {
		if d.validate == nil {
			if d.re.Match(body) {
				out = append(out, Finding{Kind: d.kind})
			}
			continue
		}
		for _, candidate := range d.re.FindAll(body, -1) {
			if d.validate(string(candidate)) {
				out = append(out, Finding{Kind: d.kind})
				break
			}
		}
	}
	return out
}

// Kinds returns the finding kinds in order (for audit messages —
// kinds only, never content).
func Kinds(findings []Finding) []string {
	kinds := make([]string, 0, len(findings))
	for _, f := range findings {
		kinds = append(kinds, f.Kind)
	}
	return kinds
}

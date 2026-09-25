package jev

import (
	"errors"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/decision/remote"
)

func allowedMap() map[string]string {
	return map[string]string{"c0": "p1/model-a", "c1": "p2/model-b"}
}

func reasonOf(t *testing.T, err error) decision.ReasonCode {
	t.Helper()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	var re *remote.Error
	if !errors.As(err, &re) {
		t.Fatalf("want typed remote error, got %T (%v)", err, err)
	}
	return re.DecisionReason()
}

func TestParseValidSelection(t *testing.T) {
	physical, conf, err := ParseResponse([]byte(`{"code":0,"message":"ok","data":{"decision":"c1","confidence":0.86,"probabilities":{"c0":0.14,"c1":0.86},"guidance":"ignored free text"}}`), allowedMap())
	if err != nil {
		t.Fatal(err)
	}
	if physical != "p2/model-b" || conf != 0.86 {
		t.Fatalf("physical=%q conf=%v", physical, conf)
	}
}

func TestParseMissingConfidenceIsUnknown(t *testing.T) {
	physical, conf, err := ParseResponse([]byte(`{"code":0,"message":"ok","data":{"decision":"c0"}}`), allowedMap())
	if err != nil {
		t.Fatal(err)
	}
	if physical != "p1/model-a" || conf != 0 {
		t.Fatalf("physical=%q conf=%v (want 0 unknown)", physical, conf)
	}
}

func TestParseMalformedResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want decision.ReasonCode
	}{
		{"empty", ``, decision.ReasonExternalInvalidResponse},
		{"invalid JSON", `{nope`, decision.ReasonExternalInvalidResponse},
		{"wrong envelope", `[1,2]`, decision.ReasonExternalInvalidResponse},
		{"missing code", `{"message":"ok","data":{"decision":"c0"}}`, decision.ReasonExternalInvalidResponse},
		{"string code", `{"code":"0","data":{"decision":"c0"}}`, decision.ReasonExternalInvalidResponse},
		{"nonzero code", `{"code":429,"message":"slow down","data":{"decision":"c0"}}`, decision.ReasonExternalInvalidResponse},
		{"missing data", `{"code":0,"message":"ok"}`, decision.ReasonExternalInvalidResponse},
		{"data not object", `{"code":0,"data":[1]}`, decision.ReasonExternalInvalidResponse},
		{"missing selection", `{"code":0,"data":{"confidence":0.5}}`, decision.ReasonExternalInvalidResponse},
		{"empty selection", `{"code":0,"data":{"decision":""}}`, decision.ReasonExternalInvalidResponse},
		{"unknown choice", `{"code":0,"data":{"decision":"c9"}}`, decision.ReasonExternalUnknownCandidate},
		{"physical choice rejected", `{"code":0,"data":{"decision":"p1/model-a"}}`, decision.ReasonExternalUnknownCandidate},
		{"confidence high", `{"code":0,"data":{"decision":"c0","confidence":1.5}}`, decision.ReasonExternalInvalidResponse},
		{"confidence low", `{"code":0,"data":{"decision":"c0","confidence":-0.2}}`, decision.ReasonExternalInvalidResponse},
		{"confidence NaN literal", `{"code":0,"data":{"decision":"c0","confidence":NaN}}`, decision.ReasonExternalInvalidResponse},
		{"prob unknown choice", `{"code":0,"data":{"decision":"c0","probabilities":{"c0":0.5,"c9":0.5}}}`, decision.ReasonExternalInvalidResponse},
		{"prob out of range", `{"code":0,"data":{"decision":"c0","probabilities":{"c0":7}}}`, decision.ReasonExternalInvalidResponse},
	}
	for _, c := range cases {
		_, _, err := ParseResponse([]byte(c.body), allowedMap())
		if got := reasonOf(t, err); got != c.want {
			t.Fatalf("%s: reason=%q want %q", c.name, got, c.want)
		}
	}
}

func TestParseNeverReturnsOutsideMapping(t *testing.T) {
	bodies := []string{
		`{"code":0,"data":{"decision":"c0"}}`,
		`{"code":0,"data":{"decision":"c1","confidence":1}}`,
	}
	for _, b := range bodies {
		physical, _, err := ParseResponse([]byte(b), allowedMap())
		if err != nil {
			t.Fatal(err)
		}
		if physical != "p1/model-a" && physical != "p2/model-b" {
			t.Fatalf("physical=%q outside mapping", physical)
		}
	}
}

func TestParseIgnoresGuidanceEverywhere(t *testing.T) {
	const canary = "GUIDANCE_CANARY_9f3e"
	_, _, err := ParseResponse([]byte(`{"code":0,"message":"`+canary+`","data":{"decision":"c9","guidance":"`+canary+`"}}`), allowedMap())
	if err == nil || !strings.Contains(err.Error(), "unknown candidate") {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("guidance leaked into error: %v", err)
	}
}

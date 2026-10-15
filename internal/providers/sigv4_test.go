package providers

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The published AWS SigV4 documentation example: signing a ListUsers
// request must reproduce the documented signature exactly.
func TestSigV4MatchesAWSDocumentationVector(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet,
		"https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	creds := sigV4Credentials{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	when := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	signV4(req, nil, creds, "us-east-1", "iam", when)

	auth := req.Header.Get("Authorization")
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, " +
		"Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if auth != want {
		t.Fatalf("Authorization:\n got %s\nwant %s", auth, want)
	}
	if req.Header.Get("X-Amz-Date") != "20150830T123600Z" {
		t.Fatalf("X-Amz-Date=%q", req.Header.Get("X-Amz-Date"))
	}
}

func TestSigV4SessionTokenAndBodyHash(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-v1/converse",
		strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	creds := sigV4Credentials{AccessKey: "AK", SecretKey: "SK", SessionToken: "TOK"}
	signV4(req, []byte(`{"messages":[]}`), creds, "us-east-1", "bedrock", time.Now())
	if req.Header.Get("X-Amz-Security-Token") != "TOK" {
		t.Fatal("session token header missing")
	}
	if !strings.Contains(req.Header.Get("Authorization"), "Credential=AK/") ||
		!strings.Contains(req.Header.Get("Authorization"), "/bedrock/aws4_request") {
		t.Fatalf("authorization scope wrong: %s", req.Header.Get("Authorization"))
	}
}

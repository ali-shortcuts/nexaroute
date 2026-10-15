package providers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Minimal AWS Signature Version 4 signer (M5-1b), used by the
// Bedrock adapter. Hand-rolled on the standard library so the
// gateway takes no AWS SDK dependency for one service; correctness
// is pinned against AWS's published documentation test vector in
// sigv4_test.go.

type sigV4Credentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string // optional
}

func sigV4HashSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sigV4HMAC(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// sigV4Escape applies SigV4 URI encoding to one path segment:
// unreserved characters (A-Z a-z 0-9 - . _ ~) pass through, every
// other byte is %XX with uppercase hex.
func sigV4Escape(segment string) string {
	var b strings.Builder
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// sigV4CanonicalURI re-encodes each segment of an escaped path the
// SigV4 way (the wire path and the signed path must agree).
func sigV4CanonicalURI(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	parts := strings.Split(escapedPath, "/")
	for i, p := range parts {
		// p is already percent-encoded; decode-then-encode would be a
		// round trip through bytes — segments reaching here are built
		// from raw model ids by the adapter, so encode directly.
		parts[i] = sigV4Escape(percentDecodeSegment(p))
	}
	return strings.Join(parts, "/")
}

func percentDecodeSegment(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := hex.DecodeString(s[i+1 : i+3]); err == nil {
				b = append(b, v...)
				i += 2
				continue
			}
		}
		b = append(b, s[i])
	}
	return string(b)
}

// signV4 signs req in place: it sets X-Amz-Date, the session token
// header when present, and the Authorization header. The body is
// hashed as given (callers pass the exact bytes being sent).
func signV4(req *http.Request, body []byte, creds sigV4Credentials, region, service string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	// Canonical headers: host + every header already set on the
	// request (sorted, lowercased, values trimmed/collapsed).
	headerKeys := []string{"host"}
	for k := range req.Header {
		lk := strings.ToLower(k)
		if lk != "host" {
			headerKeys = append(headerKeys, lk)
		}
	}
	sort.Strings(headerKeys)
	var canonicalHeaders strings.Builder
	for _, k := range headerKeys {
		var v string
		if k == "host" {
			v = req.URL.Host
		} else {
			v = strings.Join(req.Header.Values(k), ",")
		}
		v = strings.Join(strings.Fields(v), " ")
		canonicalHeaders.WriteString(k + ":" + v + "\n")
	}
	signedHeaders := strings.Join(headerKeys, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		sigV4CanonicalURI(req.URL.EscapedPath()),
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		sigV4HashSHA256(body),
	}, "\n")

	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" +
		sigV4HashSHA256([]byte(canonicalRequest))

	kDate := sigV4HMAC([]byte("AWS4"+creds.SecretKey), []byte(dateStamp))
	kRegion := sigV4HMAC(kDate, []byte(region))
	kService := sigV4HMAC(kRegion, []byte(service))
	kSigning := sigV4HMAC(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(sigV4HMAC(kSigning, []byte(stringToSign)))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+creds.AccessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

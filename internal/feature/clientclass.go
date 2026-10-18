package feature

import "strings"

// ClientClass is a bounded classification of the HTTP client family derived
// from the User-Agent header. It is a privacy-safe structural signal: only
// the coarse family is recorded, never the raw User-Agent string.
type ClientClass string

const (
	ClientClassUnknown ClientClass = "unknown"
	ClientClassWeb     ClientClass = "web"   // browsers
	ClientClassCLI     ClientClass = "cli"   // curl, wget, httpie
	ClientClassSDK     ClientClass = "sdk"   // vendor SDKs
	ClientClassAgent   ClientClass = "agent" // coding agents (claude-code, codex, opencode, ...)
)

// ClassifyClient maps a User-Agent header to a bounded client class.
// Deterministic and total: empty or unrecognized input => unknown.
func ClassifyClient(ua string) ClientClass {
	l := strings.ToLower(strings.TrimSpace(ua))
	if l == "" {
		return ClientClassUnknown
	}
	// Coding agents first: their UAs often embed generic tokens too.
	for _, token := range []string{"claude-code", "codex", "opencode", "aider", "cursor", "windsurf", "continue.dev", "cody", "copilot"} {
		if strings.Contains(l, token) {
			return ClientClassAgent
		}
	}
	for _, token := range []string{"mozilla", "chrome", "safari", "firefox", "edg/", "opr/"} {
		if strings.Contains(l, token) {
			return ClientClassWeb
		}
	}
	for _, token := range []string{"curl", "wget", "httpie", "aria2"} {
		if strings.Contains(l, token) {
			return ClientClassCLI
		}
	}
	for _, token := range []string{"sdk", "openai-python", "openai-node", "anthropic-sdk", "go-http-client", "python-httpx", "python-requests"} {
		if strings.Contains(l, token) {
			return ClientClassSDK
		}
	}
	return ClientClassUnknown
}

// Valid reports whether the class is one of the declared values.
func (c ClientClass) Valid() bool {
	switch c {
	case ClientClassUnknown, ClientClassWeb, ClientClassCLI, ClientClassSDK, ClientClassAgent:
		return true
	default:
		return false
	}
}

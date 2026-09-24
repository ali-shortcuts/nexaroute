// Package buildinfo carries identity constants that must agree across the
// binary (`--version`, startup log) and the HTTP surfaces (`/api/hello`,
// `/version`, admin snapshot). Keeping one constant here prevents the release
// version from drifting between the CLI and the gateway metadata.
package buildinfo

// Version is the NexaRoute release version reported by the HTTP surfaces
// (/api/hello, /version, the admin snapshot) and therefore by the dashboard.
// The CLI keeps its own literal in cmd/gateway/main.go because the CI installer
// smoke job scrapes it from the source; TestVersionMatchesBuildinfo pins the two
// together. Bump both and CHANGELOG.md on release.
const Version = "0.5.2"

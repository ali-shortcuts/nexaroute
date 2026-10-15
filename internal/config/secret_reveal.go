package config

import (
	"net"
	"strings"
)

// ListenIsLoopback reports whether a listen address binds only to a loopback
// interface. An empty host (":8080"), a wildcard host ("0.0.0.0", "::") and any
// hostname other than "localhost" are NOT loopback, because they can accept
// connections from other machines.
func ListenIsLoopback(listen string) bool {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return false
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		// A bare host without a port, for example "127.0.0.1" or "::1".
		host = listen
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SecretRevealAllowed reports whether the admin API may return a saved key on
// explicit request. An explicit admin.allow_secret_reveal always wins. When it
// is unset the default is true only for a loopback-only listener, which is the
// single-operator local setup; any other listener defaults to false.
func (c Config) SecretRevealAllowed() bool {
	if c.Admin.AllowSecretReveal != nil {
		return *c.Admin.AllowSecretReveal
	}
	return ListenIsLoopback(c.Listen)
}

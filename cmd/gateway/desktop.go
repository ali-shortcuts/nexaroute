package main

import (
	"net"
	"os"
	"path/filepath"
)

func defaultConfigPath() string {
	if p := os.Getenv("NEXAROUTE_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	// main reports an actionable error; never fall back to the working directory.
	if err != nil || !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Join(dir, "nexaroute", "config.json")
}

func uiURL(addr net.Addr) string {
	host, port, _ := net.SplitHostPort(addr.String())
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

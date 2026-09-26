package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

var preferredBrowsers = []string{
	"google-chrome",
	"google-chrome-stable",
	"chromium",
	"chromium-browser",
	"xdg-open",
}

type cmdStarter func(name string, args ...string) error

var (
	lookPath                = exec.LookPath
	startCommand cmdStarter = startDetached
	getenv                  = os.Getenv
)

func startDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Start()
}

func publicUIURL(listen string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		if listen == "" {
			return "http://127.0.0.1:8080/"
		}
		return "http://" + listen + "/"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

func browserLaunchDisabled() bool {
	v := strings.TrimSpace(getenv("NEXAROUTE_NO_BROWSER"))
	if v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes") {
		return true
	}
	if strings.TrimSpace(getenv("DISPLAY")) == "" && strings.TrimSpace(getenv("WAYLAND_DISPLAY")) == "" {
		return true
	}
	return false
}

func waitHTTPReady(url string, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	client := &http.Client{Timeout: 300 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	health := strings.TrimRight(url, "/") + "/healthz"
	for time.Now().Before(deadline) {
		resp, err := client.Get(health)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// openUI tries preferred browsers without blocking the caller. It never
// returns a fatal error: headless environments just print the URL.
func openUI(url string, out io.Writer) {
	if url == "" {
		return
	}
	if !strings.HasSuffix(url, "/") {
		url += "/"
	}
	if out == nil {
		out = os.Stderr
	}
	fmt.Fprintf(out, "NexaRoute UI: %s\n", url)
	if browserLaunchDisabled() {
		fmt.Fprintln(out, "Browser auto-launch skipped (headless or NEXAROUTE_NO_BROWSER).")
		return
	}
	for _, name := range preferredBrowsers {
		path, err := lookPath(name)
		if err != nil || path == "" {
			continue
		}
		if err := startCommand(path, url); err != nil {
			continue
		}
		fmt.Fprintf(out, "Opened UI with %s\n", name)
		return
	}
	fmt.Fprintln(out, "No local browser was available; open the URL above manually.")
}

func launchUIWhenReady(url string, out io.Writer) {
	if waitHTTPReady(url, 15*time.Second) {
		openUI(url, out)
		return
	}
	if out == nil {
		out = os.Stderr
	}
	fmt.Fprintf(out, "NexaRoute UI: %s (gateway not yet answering healthz)\n", url)
}

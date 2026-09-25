package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"runtime"
	"time"
)

func dashboardURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1:8080/"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}
func openDashboard(parent context.Context, url string) {
	var commands [][]string
	switch runtime.GOOS {
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		commands = [][]string{{"google-chrome", url}, {"google-chrome-stable", url}, {"chromium", url}, {"chromium-browser", url}, {"xdg-open", url}}
	case "darwin":
		commands = [][]string{{"open", "-a", "Google Chrome", url}, {"open", url}}
	case "windows":
		commands = [][]string{{"rundll32", "url.dll,FileProtocolHandler", url}}
	default:
		return
	}
	for _, args := range commands {
		if _, err := exec.LookPath(args[0]); err != nil {
			continue
		}
		// Reap the opener without tying the browser lifetime to gateway shutdown.
		cmd := exec.Command(args[0], args[1:]...)
		if err := cmd.Start(); err != nil {
			continue
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		timer := time.NewTimer(2 * time.Second)
		select {
		case err := <-done:
			timer.Stop()
			if err == nil {
				return
			}
		case <-timer.C:
			return
		case <-parent.Done():
			timer.Stop()
			return
		}
	}
}

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("NexaRoute videogen v" + version)
		return
	}
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	if code := run(os.Args[1:], os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseURL := fs.String("base-url", defaultBaseURL(), "NexaRoute base URL (default: NEXAROUTE_URL or http://127.0.0.1:8080)")
	tokenEnv := fs.String("token-env", "NEXAROUTE_VIDEO_TOKEN", "environment variable containing the Video Gateway bearer token")
	file := fs.String("file", "", "VideoRequest JSON file (create only)")
	id := fs.String("id", "", "video job ID (job-get/job-cancel only)")
	dryRun := fs.Bool("dry-run", false, "ask the gateway to estimate only; do not submit a provider job")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		printUsage(stderr)
		return 2
	}
	switch command {
	case "providers", "create", "job-get", "job-cancel":
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", command)
		printUsage(stderr)
		return 2
	}
	if command == "create" && strings.TrimSpace(*file) == "" {
		fmt.Fprintln(stderr, "create requires --file")
		return 2
	}
	if (command == "job-get" || command == "job-cancel") && strings.TrimSpace(*id) == "" {
		fmt.Fprintf(stderr, "%s requires --id\n", command)
		return 2
	}
	if strings.TrimSpace(*tokenEnv) == "" {
		fmt.Fprintln(stderr, "--token-env must name an environment variable")
		return 2
	}
	token := os.Getenv(*tokenEnv)
	if strings.TrimSpace(token) == "" {
		fmt.Fprintf(stderr, "Video Gateway token is missing: set environment variable %s (the value is never printed)\n", *tokenEnv)
		return 1
	}
	client, err := newAPIClient(*baseURL, token, nil)
	if err != nil {
		fmt.Fprintf(stderr, "configure Video Gateway client: %v\n", err)
		return 1
	}

	var result any
	switch command {
	case "providers":
		result, err = client.Providers(runContext())
	case "create":
		var data []byte
		data, err = os.ReadFile(*file)
		if err == nil {
			var req video.VideoRequest
			err = json.Unmarshal(data, &req)
			if err == nil {
				req.DryRun = *dryRun
				result, err = client.CreateJob(runContext(), req)
			}
		}
	case "job-get":
		result, err = client.GetJob(runContext(), *id)
	case "job-cancel":
		result, err = client.CancelJob(runContext(), *id)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		fmt.Fprintf(stderr, "encode result: %v\n", err)
		return 1
	}
	return 0
}

func defaultBaseURL() string {
	if value := strings.TrimSpace(os.Getenv("NEXAROUTE_URL")); value != "" {
		return value
	}
	return "http://127.0.0.1:8080"
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: videogen providers|create|job-get|job-cancel [flags]")
	fmt.Fprintln(w, "common flags: --base-url URL --token-env ENV_NAME")
	fmt.Fprintln(w, "create flags: --file REQUEST.json [--dry-run]")
	fmt.Fprintln(w, "job-get/job-cancel flags: --id JOB_ID")
}

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	videoaudio "github.com/ali-shortcuts/nexaroute/internal/video/audio"
	"github.com/ali-shortcuts/nexaroute/internal/video/composer"
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
	file := fs.String("file", "", "JSON input file (create: VideoRequest; compose: EpisodeSpec)")
	textPath := fs.String("text-file", "", "UTF-8 narration text file (narrate only)")
	modelPath := fs.String("model", "", "local Piper ONNX voice model path (narrate only)")
	outputPath := fs.String("out", "", "output path (asset-download/narrate only)")
	piperBin := fs.String("piper-bin", "piper", "Piper executable path (narrate only)")
	id := fs.String("id", "", "video job ID (job-get/job-cancel/asset-download)")
	assetID := fs.String("asset-id", "", "stored asset ID (asset-download only)")
	outPath := fs.String("out", "", "local output file (asset-download only)")
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
	case "providers", "create", "job-get", "job-cancel", "asset-download", "compose", "narrate":
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", command)
		printUsage(stderr)
		return 2
	}
	if (command == "create" || command == "compose") && strings.TrimSpace(*file) == "" {
		fmt.Fprintf(stderr, "%s requires --file\n", command)
		return 2
	}
	if command == "narrate" {\n\t\tif strings.TrimSpace(*textPath) == "" || strings.TrimSpace(*modelPath) == "" || strings.TrimSpace(*outputPath) == "" {\n\t\t\tfmt.Fprintln(stderr, "narrate requires --text-file, --model, and --out")\n\t\t\treturn 2\n\t\t}\n\t\tif err := videoaudio.SynthesizePiper(context.Background(), videoaudio.PiperOptions{Binary: *piperBin, TextPath: *textPath, ModelPath: *modelPath, Output: *outputPath}); err != nil {\n\t\t\tfmt.Fprintf(stderr, "narrate: %v\\n", err)\n\t\t\treturn 1\n\t\t}\n\t\tfmt.Fprintf(stdout, "Narration written to %s\\n", *outputPath)\n\t\treturn 0\n\t}\n\tif command == "compose" {
		data, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintf(stderr, "compose: read episode manifest: %v\n", err)
			return 1
		}
		var spec composer.EpisodeSpec
		if err := json.Unmarshal(data, &spec); err != nil {
			fmt.Fprintf(stderr, "compose: decode episode manifest: %v\n", err)
			return 1
		}
		runner := composer.New(defaultFFmpegBinary(), "")
		result, err := runner.ComposeEpisode(context.Background(), spec)
		if err != nil {
			fmt.Fprintf(stderr, "compose: %v\n", err)
			return 1
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			fmt.Fprintf(stderr, "encode compose result: %v\n", err)
			return 1
		}
		return 0
	}
	if (command == "job-get" || command == "job-cancel" || command == "asset-download") && strings.TrimSpace(*id) == "" {
		fmt.Fprintf(stderr, "%s requires --id\n", command)
		return 2
	}
	if command == "asset-download" {
		if strings.TrimSpace(*assetID) == "" {
			fmt.Fprintln(stderr, "asset-download requires --asset-id")
			return 2
		}
		if strings.TrimSpace(*outPath) == "" {
			fmt.Fprintln(stderr, "asset-download requires --out")
			return 2
		}
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
		result, err = client.Providers(context.Background())
	case "create":
		var data []byte
		data, err = os.ReadFile(*file)
		if err == nil {
			var req video.VideoRequest
			err = json.Unmarshal(data, &req)
			if err == nil {
				req.DryRun = req.DryRun || *dryRun
				result, err = client.CreateJob(context.Background(), req)
			}
		}
	case "job-get":
		result, err = client.GetJob(context.Background(), *id)
	case "job-cancel":
		result, err = client.CancelJob(context.Background(), *id)
	case "asset-download":
		err = client.DownloadAsset(context.Background(), *id, *assetID, *outPath)
		if err == nil {
			fmt.Fprintf(stdout, "Downloaded asset to %s\n", *outPath)
			return 0
		}
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

func defaultFFmpegBinary() string {
	if value := strings.TrimSpace(os.Getenv("NEXAROUTE_FFMPEG")); value != "" {
		return value
	}
	return "ffmpeg"
}

func defaultBaseURL() string {
	if value := strings.TrimSpace(os.Getenv("NEXAROUTE_URL")); value != "" {
		return value
	}
	return "http://127.0.0.1:8080"
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: videogen providers|create|job-get|job-cancel|asset-download|compose|narrate [flags]")
	fmt.Fprintln(w, "common flags: --base-url URL --token-env ENV_NAME")
	fmt.Fprintln(w, "create flags: --file REQUEST.json [--dry-run]")
	fmt.Fprintln(w, "job-get/job-cancel flags: --id JOB_ID")\n\tfmt.Fprintln(w, "narrate flags: --text-file story.txt --model voice.onnx --out narration.wav [--piper-bin piper]")
	fmt.Fprintln(w, "asset-download flags: --id JOB_ID --asset-id ASSET_ID --out FILE")
	fmt.Fprintln(w, "compose flags: --file EPISODE.json (local FFmpeg; no Gateway token required)")
}

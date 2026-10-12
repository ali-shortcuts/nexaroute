package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/video"
	videoaudio "github.com/ali-shortcuts/nexaroute/internal/video/audio"
	"github.com/ali-shortcuts/nexaroute/internal/video/composer"
)

type renderManifest struct {
	composer.EpisodeSpec
	NarrationTextPath   string `json:"narration_text_path,omitempty"`
	PiperModelPath      string `json:"piper_model_path,omitempty"`
	NarrationOutputPath string `json:"narration_output_path,omitempty"`
}

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
	piperBin := fs.String("piper-bin", "piper", "Piper executable path (narrate only)")
	id := fs.String("id", "", "video job ID (job-get/job-cancel/asset-download)")
	assetID := fs.String("asset-id", "", "stored asset ID (asset-download only)")
	outPath := fs.String("out", "", "local output file (asset-download/narrate only)")
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
	case "providers", "create", "job-get", "job-cancel", "asset-download", "compose", "narrate", "render":
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", command)
		printUsage(stderr)
		return 2
	}
	if (command == "create" || command == "compose" || command == "render") && strings.TrimSpace(*file) == "" {
		fmt.Fprintf(stderr, "%s requires --file\n", command)
		return 2
	}
	if command == "render" {
		data, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintf(stderr, "render: read episode manifest: %v\n", err)
			return 1
		}
		var spec renderManifest
		if err := json.Unmarshal(data, &spec); err != nil {
			fmt.Fprintf(stderr, "render: decode episode manifest: %v\n", err)
			return 1
		}
		hasNarrationText := strings.TrimSpace(spec.NarrationTextPath) != ""
		hasPiperModel := strings.TrimSpace(spec.PiperModelPath) != ""
		hasNarrationOutput := strings.TrimSpace(spec.NarrationOutputPath) != ""
		if hasNarrationText || hasPiperModel || hasNarrationOutput {
			if !hasNarrationText || !hasPiperModel || !hasNarrationOutput {
				fmt.Fprintln(stderr, "render: narration_text_path, piper_model_path, and narration_output_path must be provided together")
				return 2
			}
			if strings.TrimSpace(spec.NarrationPath) != "" {
				fmt.Fprintln(stderr, "render: use either narration_path or local Piper synthesis fields, not both")
				return 2
			}
			for _, source := range append(append(append([]string{}, spec.Shots...), spec.MusicPath, spec.SubtitlePath), spec.OutputPath) {
				if strings.TrimSpace(source) != "" && sameCleanPath(source, spec.NarrationOutputPath) {
					fmt.Fprintf(stderr, "render: narration_output_path must not overwrite episode input or output %q\n", source)
					return 2
				}
			}
			if err := videoaudio.SynthesizePiper(context.Background(), videoaudio.PiperOptions{Binary: *piperBin, TextPath: spec.NarrationTextPath, ModelPath: spec.PiperModelPath, Output: spec.NarrationOutputPath}); err != nil {
				fmt.Fprintf(stderr, "render: narration synthesis failed: %v\n", err)
				return 1
			}
			spec.NarrationPath = spec.NarrationOutputPath
		}
		runner := composer.New(defaultFFmpegBinary(), "")
		result, err := runner.ComposeEpisode(context.Background(), spec.EpisodeSpec)
		if err != nil {
			fmt.Fprintf(stderr, "render: compose episode: %v\n", err)
			return 1
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"narration_path": spec.NarrationPath, "composition": result}); err != nil {
			fmt.Fprintf(stderr, "encode render result: %v\n", err)
			return 1
		}
		return 0
	}
	if command == "narrate" {
		if strings.TrimSpace(*textPath) == "" || strings.TrimSpace(*modelPath) == "" || strings.TrimSpace(*outPath) == "" {
			fmt.Fprintln(stderr, "narrate requires --text-file, --model, and --out")
			return 2
		}
		if err := videoaudio.SynthesizePiper(context.Background(), videoaudio.PiperOptions{Binary: *piperBin, TextPath: *textPath, ModelPath: *modelPath, Output: *outPath}); err != nil {
			fmt.Fprintf(stderr, "narrate: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "Narration written to %s\n", *outPath)
		return 0
	}
	if command == "compose" {
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


func sameCleanPath(a, b string) bool {
	absA, errA := filepath.Abs(strings.TrimSpace(a))
	absB, errB := filepath.Abs(strings.TrimSpace(b))
	return errA == nil && errB == nil && filepath.Clean(absA) == filepath.Clean(absB)
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
	fmt.Fprintln(w, "usage: videogen providers|create|job-get|job-cancel|asset-download|compose|narrate|render [flags]")
	fmt.Fprintln(w, "common flags: --base-url URL --token-env ENV_NAME")
	fmt.Fprintln(w, "create flags: --file REQUEST.json [--dry-run]")
	fmt.Fprintln(w, "job-get/job-cancel flags: --id JOB_ID")
	fmt.Fprintln(w, "narrate flags: --text-file story.txt --model voice.onnx --out narration.wav [--piper-bin piper]")
	fmt.Fprintln(w, "render flags: --file EPISODE.json [--piper-bin piper] (local narration + composition)")
	fmt.Fprintln(w, "asset-download flags: --id JOB_ID --asset-id ASSET_ID --out FILE")
	fmt.Fprintln(w, "compose flags: --file EPISODE.json (local FFmpeg; no Gateway token required)")
}

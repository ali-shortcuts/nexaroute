package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/ali-shortcuts/nexaroute/internal/video"
	"github.com/ali-shortcuts/nexaroute/internal/video/cost"
	"github.com/ali-shortcuts/nexaroute/internal/video/orchestrator"
	"github.com/ali-shortcuts/nexaroute/internal/video/providers"
	"github.com/ali-shortcuts/nexaroute/internal/video/queue"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("NexaRoute videogen v" + version)
		return
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: videogen create|job-get")
		os.Exit(2)
	}
	ctx := context.Background()
	store := queue.NewMemoryStore()
	q := queue.New(8)
	fake := providers.NewFake(2)
	o := &orchestrator.Orchestrator{Store: store, Queue: q, Providers: orchestrator.Registry{"fake": fake}, Ledger: &cost.Ledger{}}
	switch os.Args[1] {
	case "create":
		fs := flag.NewFlagSet("create", flag.ExitOnError)
		file := fs.String("file", "", "request JSON file")
		dry := fs.Bool("dry-run", false, "estimate only")
		_ = fs.Parse(os.Args[2:])
		if *file == "" {
			fmt.Fprintln(os.Stderr, "--file is required")
			os.Exit(2)
		}
		data, err := os.ReadFile(*file)
		if err != nil {
			panic(err)
		}
		var r video.VideoRequest
		if err = json.Unmarshal(data, &r); err != nil {
			panic(err)
		}
		r.DryRun = *dry
		if r.ProviderPreference == "" {
			r.ProviderPreference = "fake"
		}
		j, err := o.Create(ctx, r)
		if err != nil {
			panic(err)
		}
		printJSON(j)
	case "job-get":
		fmt.Fprintln(os.Stderr, "job-get requires a configured durable store in the gateway")
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		os.Exit(2)
	}
}
func printJSON(v any) { b, _ := json.MarshalIndent(v, "", "  "); fmt.Println(string(b)) }

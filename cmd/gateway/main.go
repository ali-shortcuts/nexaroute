package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/logging"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

var version = "0.6.0"

func ensureConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	cfg := config.Default()
	return config.SaveAtomic(path, cfg)
}

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to JSON config")
	noBrowser := flag.Bool("no-browser", false, "do not automatically open the Web UI")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("NexaRoute v" + version)
		return
	}
	bootstrap := log.New(os.Stderr, "nexaroute ", log.LstdFlags|log.Lmicroseconds)
	if *configPath == "" {
		bootstrap.Fatal("cannot locate user config directory; set HOME, XDG_CONFIG_HOME, or NEXAROUTE_CONFIG")
	}
	absolute, err := filepath.Abs(*configPath)
	if err != nil {
		bootstrap.Fatal(err)
	}
	// Resolve aliases before taking the sibling lock.
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		bootstrap.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		bootstrap.Fatal(err)
	}
	absolute = filepath.Join(dir, filepath.Base(absolute))
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	*configPath = absolute
	lock, err := lockInstance(*configPath)
	if err != nil {
		bootstrap.Fatal(err)
	}
	defer lock.Close()
	if err := ensureConfig(*configPath); err != nil {
		bootstrap.Fatal(err)
	}
	if err := config.RemoveStaleBackup(*configPath); err != nil {
		bootstrap.Fatal(err)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		bootstrap.Fatal(err)
	}

	var logWriters []io.Writer
	var logFile *logging.RotatingWriter
	logPath := strings.TrimSpace(cfg.Logging.File)
	if logPath != "off" {
		if logPath == "" || logPath == "auto" {
			logPath = filepath.Join(filepath.Dir(*configPath), "nexaroute.log")
		} else if !filepath.IsAbs(logPath) {
			logPath = filepath.Join(filepath.Dir(*configPath), logPath)
		}
		logFile, err = logging.NewRotatingWriter(logPath, int64(cfg.Logging.MaxSizeMB)<<20, cfg.Logging.MaxBackups)
		if err != nil {
			bootstrap.Printf("log_file_error=%v; continuing with rate-limited console logging", err)
		} else {
			defer logFile.Close()
			logWriters = append(logWriters, logFile)
		}
	}
	if cfg.Logging.ConsoleMaxLinesPerMinute > 0 {
		logWriters = append(logWriters, logging.NewRateLimitedWriter(os.Stderr, cfg.Logging.ConsoleMaxLinesPerMinute, time.Minute))
	}
	var logOutput io.Writer = io.Discard
	if len(logWriters) == 0 {
		// file: "off" combined with console_max_lines_per_minute: 0 silences
		// every log line, including shutdown errors. Warn once on stderr so
		// an operator notices instead of running blind.
		fmt.Fprintf(os.Stderr, "nexaroute: warning: all logging is disabled (logging.file=off and console_max_lines_per_minute=0)\n")
	}
	if len(logWriters) == 1 {
		logOutput = logWriters[0]
	} else if len(logWriters) > 1 {
		logOutput = logging.NewFanoutWriter(logWriters...)
	}
	logger := log.New(logOutput, "nexaroute ", log.LstdFlags|log.Lmicroseconds)
	if logFile != nil {
		logger.Printf("bounded_log_file=%s max_mb=%d backups=%d", logPath, cfg.Logging.MaxSizeMB, cfg.Logging.MaxBackups)
	}
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.ConfigureProviderIncidents(cfg.Routing.ProviderFailureThreshold, cfg.ProviderFailureWindow(), cfg.ProviderCooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		logger.Fatal(err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(500)
	pe := probe.New(cfg, reg, rt, hm, bus)
	api := httpapi.New(cfg, *configPath, reg, rt, hm, bus, pe, logger)
	api.SyncCapabilityContracts()
	pe.SetCapabilityStore(api.CapabilityStore())
	srv := &http.Server{
		Addr: cfg.Listen, Handler: api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       180 * time.Second,
		MaxHeaderBytes:    128 << 10,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		bootstrap.Fatalf("cannot listen on %s (another instance may be running): %v", cfg.Listen, err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() {
		logger.Printf("version=%s config=%s listening=http://%s", version, *configPath, cfg.Listen)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			serverErr <- err
			cancel()
		}
	}()
	url := uiURL(listener.Addr())
	fmt.Fprintf(os.Stderr, "NexaRoute UI: %s\nConfig: %s\nPress Ctrl+C to stop.\n", url, *configPath)
	go func() {
		readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
		defer readyCancel()
		if err := waitForUI(readyCtx, url); err != nil {
			if ctx.Err() == nil {
				bootstrap.Printf("UI readiness check failed: %v; open %s manually", err, url)
			}
			return
		}
		if !*noBrowser {
			if err := openBrowser(ctx, url); err != nil && ctx.Err() == nil {
				bootstrap.Printf("%v; open %s manually", err, url)
			}
		}
	}()
	if cfg.Probe.Enabled && cfg.Probe.OnStart {
		result := pe.Prime(ctx)
		logger.Printf("startup_probe total=%d ready=%d failed=%d cooldown=%d duration_ms=%d", result.Total, result.Passed, result.Failed, result.SkippedCooldown, result.DurationMS)
	}
	pe.Start(ctx)
	<-ctx.Done()
	// The drain window must cover the longest permitted in-flight request:
	// non-streaming requests up to request_timeout_ms and streams whose idle
	// timeout can exceed it. A fixed 10s deadline would SIGTERM-cut active
	// responses mid-flight.
	drain := cfg.RequestTimeout()
	for _, p := range cfg.Providers {
		if sd := time.Duration(p.StreamIdleTimeoutSeconds) * time.Second; sd > drain {
			drain = sd
		}
	}
	if drain < 10*time.Second {
		drain = 10 * time.Second
	}
	drain += 5 * time.Second
	shutdown, shutdownCancel := context.WithTimeout(context.Background(), drain)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdown); err != nil {
		logger.Printf("shutdown_error=%v", err)
	}
	select {
	case err := <-serverErr:
		logger.Fatalf("server_error=%v", err)
	default:
	}
}

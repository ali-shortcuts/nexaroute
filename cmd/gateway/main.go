package main

import (
	"context"
	"crypto/tls"
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
	"github.com/ali-shortcuts/nexaroute/internal/desktop"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/logging"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/telemetry"
	"github.com/ali-shortcuts/nexaroute/internal/transport"
	videoDomain "github.com/ali-shortcuts/nexaroute/internal/video"
	videoRuntime "github.com/ali-shortcuts/nexaroute/internal/video/runtime"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"
var notifyGatewayContext = signal.NotifyContext

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

// dispatchSubcommand routes a known CLI subcommand and returns its exit
// code with handled=true. Anything else (gateway flags such as --config
// or --version, or no arguments) returns handled=false and main starts
// the gateway. Keeping the routing in one testable function matters:
// the `config` subcommand previously existed and was unit-tested via
// direct calls, but main never dispatched it — running
// `nexaroute config validate` silently started the gateway instead
// (proven against a binary built from main, 2026-10-15).
func dispatchSubcommand(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}
	rest := args[1:]
	switch args[0] {
	case "init":
		return runInit(rest, stdin, stdout, stderr), true
	case "privacy":
		return runPrivacy(rest, stdout, stderr), true
	case "audit":
		return runAudit(rest, stdout, stderr), true
	case "policy":
		return runPolicy(rest, stdout, stderr), true
	case "config":
		return runConfig(rest, stdout, stderr), true
	case "doctor":
		return runDoctor(rest, stdout, stderr), true
	case "provider-accept":
		return runProviderAccept(rest, stdout, stderr), true
	case "external-accept":
		return runExternalAccept(rest, stdout, stderr), true
	case "backup":
		return runBackup(rest, stdout, stderr), true
	case "restore":
		return runRestore(rest, stdout, stderr), true
	case "secrets":
		if err := runSecrets(rest); err != nil {
			fmt.Fprintln(stderr, "nexaroute: secrets command failed:", err)
			return 1, true
		}
		return 0, true
	case "conformance":
		return runConformance(rest, stdout, stderr), true
	}
	return 0, false
}

func main() {
	if code, handled := dispatchSubcommand(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		if code != 0 {
			os.Exit(code)
		}
		return
	}
	flags := flag.NewFlagSet("nexaroute", flag.ExitOnError)
	configPath := flags.String("config", defaultConfigPath(), "path to JSON config")
	noBrowser := flags.Bool("no-browser", false, "do not automatically open the Web UI")
	showVersion := flags.Bool("version", false, "print version and exit")
	_ = flags.Parse(os.Args[1:])
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

	processLock, acquired, err := desktop.Acquire(*configPath + ".lock")
	if err != nil {
		bootstrap.Fatalf("cannot acquire local gateway lock: %v", err)
	}
	if !acquired {
		uiURL := dashboardURL(*configPath)
		openExistingUI(uiURL, os.Stdout)
		return
	}
	defer processLock.Close()

	if err := ensureConfig(*configPath); err != nil {
		bootstrap.Fatal(err)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		bootstrap.Fatal(err)
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintf(os.Stderr, "nexaroute: config warning: %s\n", w)
	}
	for _, w := range cfg.EligibilityWarnings {
		fmt.Fprintf(os.Stderr, "nexaroute: config warning: %s\n", w)
	}
	cpRuntime, err := openControlPlaneRuntime(cfg.ControlPlane, *configPath)
	if err != nil {
		bootstrap.Fatalf("cannot initialize control-plane runtime: %v", err)
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
	if err := api.SecurityStoreError(); err != nil {
		bootstrap.Fatalf("cannot initialize admin security store: %v", err)
	}
	// Budget webhook alerts deduplicate across restarts on the durable
	// control-plane store when one is configured (file, postgres,
	// redis); otherwise the alerter falls back to budget_alerts
	// state_path or in-memory marks. Wiring is best-effort: store
	// errors never fail requests.
	if cpRuntime != nil && cpRuntime.manager != nil {
		api.SetBudgetAlertStore(httpapi.NewIOBudgetAlertStore(&controlPlaneBudgetAlertIO{runtime: cpRuntime}))
	}
	// Optional OTLP span export (M4-6 part B). The collector URL comes
	// only from the environment variable named in the config, mirroring
	// the control-plane backends; unset means disabled. An invalid URL
	// fails startup rather than silently dropping telemetry.
	if cfg.Telemetry.OTLPEndpointEnv != "" {
		endpoint := strings.TrimSpace(os.Getenv(cfg.Telemetry.OTLPEndpointEnv))
		if endpoint != "" {
			exporter, err := telemetry.New(endpoint, cfg.Telemetry.ServiceName)
			if err != nil {
				bootstrap.Fatalf("cannot initialize telemetry exporter: %v", err)
			}
			api.SetTelemetryExporter(exporter)
			defer exporter.Close()
			logger.Printf("telemetry_otlp_export enabled service=%s", cfg.Telemetry.ServiceName)
		}
	}
	defer api.CloseSecurityStore()
	var videoRT *videoRuntime.Runtime
	if cfg.Video.Enabled {
		// Universal video planner defaults: point the code-render planner
		// back at this gateway's own data plane (loopback) so any
		// configured text/coding model can author plans through the normal
		// routing/credential machinery, and advertise the gateway's
		// configured public/deployment model names as planning models when
		// the operator did not pin an explicit list.
		plannerBaseURL := cfg.Video.CodeRenderPlannerBaseURL
		if cfg.Video.CodeRenderEnabled && strings.TrimSpace(plannerBaseURL) == "" {
			host, port, splitErr := net.SplitHostPort(cfg.Listen)
			if splitErr == nil {
				if host == "" || host == "0.0.0.0" || host == "::" {
					host = "127.0.0.1"
				}
				plannerBaseURL = "http://" + net.JoinHostPort(host, port)
			}
		}
		plannerModels := cfg.Video.CodeRenderPlannerModels
		if cfg.Video.CodeRenderEnabled && len(plannerModels) == 0 {
			seen := map[string]bool{}
			add := func(name string) {
				name = strings.TrimSpace(name)
				if name != "" && !seen[name] {
					seen[name] = true
					plannerModels = append(plannerModels, name)
				}
			}
			add(cfg.Routing.PublicModel)
			for _, ve := range cfg.VirtualEndpoints {
				add(ve.PublicModel)
			}
			for _, p := range cfg.Providers {
				if !p.Enabled {
					continue
				}
				for _, m := range p.Models {
					if !m.Enabled {
						continue
					}
					add(m.Model)
					for _, a := range m.Aliases {
						add(a)
					}
				}
			}
		}
		videoRT, err = videoRuntime.New(videoDomain.Config{
			Enabled:                      cfg.Video.Enabled,
			StorePath:                    cfg.Video.StorePath,
			StorageRoot:                  cfg.Video.StorageRoot,
			QueueSize:                    cfg.Video.QueueSize,
			Workers:                      cfg.Video.Workers,
			AuthTokenEnv:                 cfg.Video.AuthTokenEnv,
			MaxAssetBytes:                cfg.Video.MaxAssetBytes,
			DevelopmentFakeProvider:      cfg.Video.DevelopmentFakeProvider,
			ComfyUIEnabled:               cfg.Video.ComfyUIEnabled,
			ComfyUIBaseURL:               cfg.Video.ComfyUIBaseURL,
			ComfyUIWorkflowPath:          cfg.Video.ComfyUIWorkflowPath,
			ComfyUIModel:                 cfg.Video.ComfyUIModel,
			ComfyUIMaxDurationSeconds:    cfg.Video.ComfyUIMaxDurationSeconds,
			ComfyUIPromptNodeID:          cfg.Video.ComfyUIPromptNodeID,
			ComfyUIPromptInput:           cfg.Video.ComfyUIPromptInput,
			ComfyUINegativeNodeID:        cfg.Video.ComfyUINegativeNodeID,
			ComfyUINegativeInput:         cfg.Video.ComfyUINegativeInput,
			ComfyUIWidthNodeID:           cfg.Video.ComfyUIWidthNodeID,
			ComfyUIWidthInput:            cfg.Video.ComfyUIWidthInput,
			ComfyUIHeightNodeID:          cfg.Video.ComfyUIHeightNodeID,
			ComfyUIHeightInput:           cfg.Video.ComfyUIHeightInput,
			ComfyUIFramesNodeID:          cfg.Video.ComfyUIFramesNodeID,
			ComfyUIFramesInput:           cfg.Video.ComfyUIFramesInput,
			ComfyUIFPSNodeID:             cfg.Video.ComfyUIFPSNodeID,
			ComfyUIFPSInput:              cfg.Video.ComfyUIFPSInput,
			ComfyUISeedNodeID:            cfg.Video.ComfyUISeedNodeID,
			ComfyUISeedInput:             cfg.Video.ComfyUISeedInput,
			PiperEnabled:                 cfg.Video.PiperEnabled,
			PiperBinary:                  cfg.Video.PiperBinary,
			PiperModelPath:               cfg.Video.PiperModelPath,
			CodeRenderEnabled:            cfg.Video.CodeRenderEnabled,
			CodeRenderPlannerBaseURL:     plannerBaseURL,
			CodeRenderPlannerAPIKeyEnv:   cfg.Video.CodeRenderPlannerAPIKeyEnv,
			CodeRenderPlannerModel:       cfg.Video.CodeRenderPlannerModel,
			CodeRenderPlannerModels:      plannerModels,
			CodeRenderPlannerJSONMode:    cfg.Video.CodeRenderPlannerJSONMode,
			CodeRenderMaxDurationSeconds: cfg.Video.CodeRenderMaxDurationSeconds,
			CodeRenderWorkRoot:           cfg.Video.CodeRenderWorkRoot,
			CodeRenderRetentionHours:     cfg.Video.CodeRenderRetentionHours,
		}, filepath.Dir(*configPath))
		if err != nil {
			bootstrap.Fatalf("cannot initialize video gateway: %v", err)
		}
		api.AttachVideoHandler(videoRT.Handler)
	}
	api.SyncCapabilityContracts()
	pe.SetCapabilityStore(api.CapabilityStore())
	srv := &http.Server{
		Addr: cfg.Listen, Handler: api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       180 * time.Second,
		MaxHeaderBytes:    128 << 10,
	}
	ctx, cancel := notifyGatewayContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if videoRT != nil {
		if err := videoRT.Start(ctx); err != nil {
			bootstrap.Fatalf("cannot recover video jobs: %v", err)
		}
		defer videoRT.Close()
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		bootstrap.Fatalf("cannot listen on %s (another instance may be running): %v", cfg.Listen, err)
	}
	defer listener.Close()
	secureListener := cfg.TLS.Enabled
	serverErr := make(chan error, 1)
	var serveListener net.Listener = listener
	if cfg.TLS.Enabled {
		tlsConfig, tlsErr := transport.ServerTLSConfig(cfg.TLS, filepath.Dir(*configPath))
		if tlsErr != nil {
			bootstrap.Fatalf("TLS configuration is invalid: %v", tlsErr)
		}
		srv.TLSConfig = tlsConfig
		serveListener = tls.NewListener(listener, tlsConfig)
	}
	go func() {
		scheme := "http"
		if secureListener {
			scheme = "https"
		}
		logger.Printf("version=%s config=%s listening=%s://%s", version, *configPath, scheme, cfg.Listen)
		if err := srv.Serve(serveListener); err != nil && err != http.ErrServerClosed {
			serverErr <- err
			cancel()
		}
	}()
	url := uiURLWithScheme(listener.Addr(), secureListener)
	fmt.Fprintf(os.Stderr, "NexaRoute UI: %s\nConfig: %s\nPress Ctrl+C to stop.\n", url, *configPath)
	go func() {
		readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
		defer readyCancel()
		if err := desktop.WaitReady(readyCtx, strings.TrimSuffix(url, "/")); err != nil {
			if ctx.Err() == nil {
				bootstrap.Printf("UI readiness check failed: %v; open %s manually", err, url)
			}
			return
		}
		if !*noBrowser {
			if browser, err := desktop.OpenBrowser(url, nil, nil); err != nil && ctx.Err() == nil {
				bootstrap.Printf("browser unavailable: %v; open %s manually", err, url)
			} else if err == nil {
				fmt.Fprintf(os.Stdout, "Opened NexaRoute dashboard with %s: %s\n", browser, url)
			}
		}
	}()
	if cfg.Probe.Enabled && cfg.Probe.OnStart {
		result := pe.Prime(ctx)
		logger.Printf("startup_probe total=%d ready=%d failed=%d cooldown=%d duration_ms=%d", result.Total, result.Passed, result.Failed, result.SkippedCooldown, result.DurationMS)
	}
	pe.Start(ctx)
	<-ctx.Done()
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
	if cpRuntime != nil {
		if err := cpRuntime.Close(); err != nil {
			logger.Printf("control_plane_close_error=%v", err)
		}
	}
	select {
	case err := <-serverErr:
		logger.Fatalf("server_error=%v", err)
	default:
	}
}

func dashboardURL(configPath string) string {
	cfg, err := config.Load(configPath)
	if err != nil {
		return "http://127.0.0.1:8080/"
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return "http://127.0.0.1:8080/"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

func openExistingUI(url string, output io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := desktop.WaitReady(ctx, strings.TrimSuffix(url, "/")); err != nil {
		fmt.Fprintf(output, "NexaRoute may already be starting. Dashboard: %s\n", url)
		return
	}
	if browser, err := desktop.OpenBrowser(url, nil, nil); err != nil {
		fmt.Fprintf(output, "NexaRoute is already running. Dashboard: %s (browser unavailable: %v)\n", url, err)
	} else {
		fmt.Fprintf(output, "NexaRoute is already running; opened dashboard with %s: %s\n", browser, url)
	}
}

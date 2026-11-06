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

	"github.com/ali-shortcuts/nexaroute/internal/breakersync"
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

// osExit, fatalLog and evalSymlinks are seams so main's startup-failure
// paths can be tested without killing the test process. Production
// behaviour is unchanged: osExit calls os.Exit, fatalLog calls
// Logger.Fatalf (log the message, exit 1), evalSymlinks resolves symlinks.
var osExit = os.Exit
var fatalLog = func(l *log.Logger, format string, args ...any) { l.Fatalf(format, args...) }
var evalSymlinks = filepath.EvalSymlinks
var osHostname = os.Hostname
var netListen = net.Listen

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

// logCloseError logs a deferred cleanup failure; a nil error is silent.
// It exists so the shutdown close-error branches in main share one testable
// shape instead of three inline ifs with near-unreachable error returns.
func logCloseError(logger *log.Logger, label string, err error) {
	if err != nil {
		logger.Printf("%s=%v", label, err)
	}
}

// initBreakerSync builds the cross-replica breaker syncer over the
// already-open control-plane store. It is a seam so the two failure modes
// (a store that cannot be adapted, a syncer that cannot be built) can be
// tested: neither is reachable through a valid configuration, because main
// only calls it with a live runtime, a live health manager and a non-empty
// replica id.
var initBreakerSync = func(rt *controlPlaneRuntime, hm *health.Manager, replicaID string, interval time.Duration, logf func(string, ...any)) (*breakersync.Syncer, error) {
	store, err := rt.BreakerStore(replicaID)
	if err != nil {
		return nil, err
	}
	return breakersync.New(store, hm, breakersync.Options{ReplicaID: replicaID, Interval: interval, Logf: logf})
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
	case "diagnostics":
		return runDiagnostics(rest, stdout, stderr), true
	case "provider-accept":
		return runProviderAccept(rest, stdout, stderr), true
	case "external-accept":
		return runExternalAccept(rest, stdout, stderr), true
	case "vault-accept":
		return runVaultAccept(rest, stdout, stderr), true
	case "comfyui-accept":
		return runComfyUIAccept(rest, stdout, stderr), true
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
	case "quality-gate":
		return runQualityGate(rest, stdout, stderr), true
	}
	return 0, false
}

func main() {
	if code, handled := dispatchSubcommand(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		if code != 0 {
			osExit(code)
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
		fatalLog(bootstrap, "cannot locate user config directory; set HOME, XDG_CONFIG_HOME, or NEXAROUTE_CONFIG")
	}
	absolute, err := filepath.Abs(*configPath)
	if err != nil {
		fatalLog(bootstrap, "%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		fatalLog(bootstrap, "%v", err)
	}
	dir, err := evalSymlinks(filepath.Dir(absolute))
	if err != nil {
		fatalLog(bootstrap, "%v", err)
	}
	absolute = filepath.Join(dir, filepath.Base(absolute))
	if resolved, err := evalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	*configPath = absolute

	processLock, acquired, err := desktop.Acquire(*configPath + ".lock")
	if err != nil {
		fatalLog(bootstrap, "cannot acquire local gateway lock: %v", err)
	}
	if !acquired {
		uiURL := dashboardURL(*configPath)
		openExistingUI(uiURL, os.Stdout)
		return
	}
	defer processLock.Close()

	if err := ensureConfig(*configPath); err != nil {
		fatalLog(bootstrap, "%v", err)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatalLog(bootstrap, "%v", err)
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintf(os.Stderr, "nexaroute: config warning: %s\n", w)
	}
	for _, w := range cfg.EligibilityWarnings {
		fmt.Fprintf(os.Stderr, "nexaroute: config warning: %s\n", w)
	}
	cpRuntime, err := openControlPlaneRuntime(cfg.ControlPlane, *configPath)
	if err != nil {
		fatalLog(bootstrap, "cannot initialize control-plane runtime: %v", err)
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
	var logOutput = io.Discard
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
		fatalLog(logger, "%v", err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(500)
	pe := probe.New(cfg, reg, rt, hm, bus)
	api := httpapi.New(cfg, *configPath, reg, rt, hm, bus, pe, logger)
	if err := api.SecurityStoreError(); err != nil {
		fatalLog(bootstrap, "cannot initialize admin security store: %v", err)
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
				fatalLog(bootstrap, "cannot initialize telemetry exporter: %v", err)
			}
			api.SetTelemetryExporter(exporter)
			defer exporter.Close()
			logger.Printf("telemetry_otlp_export enabled service=%s", cfg.Telemetry.ServiceName)
		}
	}
	defer func() { logCloseError(logger, "security_store_close_error", api.CloseSecurityStore()) }()
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
			JobStoreBackend:              cfg.Video.JobStoreBackend,
			JobStoreURL:                  cfg.Video.JobStoreURL,
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
			fatalLog(bootstrap, "cannot initialize video gateway: %v", err)
		}
		api.AttachVideoHandler(videoRT.Handler)
	}
	// Opt-in NexaFlow wiring (issue #765, combined wiring). Disabled by
	// default: no routes, no store, no behaviour change. Must run before
	// api.Handler() is captured below, because the mux mounts NexaFlow
	// routes only while a service is attached. The store closer runs
	// deferred after the HTTP graceful shutdown below.
	if nexaflowCloser, err := maybeAttachNexaflow(api, cfg, hm, logger); err != nil {
		fatalLog(bootstrap, "cannot initialize nexaflow service: %v", err)
	} else if nexaflowCloser != nil {
		defer func() { logCloseError(logger, "nexaflow_store_close_error", nexaflowCloser.Close()) }()
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
			fatalLog(bootstrap, "cannot recover video jobs: %v", err)
		}
		defer videoRT.Close()
	}
	// Opt-in cross-replica circuit-breaker synchronization (ADR 0003).
	// Default off; when enabled it reuses the already-open control-plane
	// store and runs entirely off the request path. A store outage is
	// logged per round and local breaker state continues untouched.
	if cfg.ControlPlane.BreakerSync && cpRuntime != nil {
		replicaID := cfg.ControlPlane.BreakerSyncReplicaID
		if replicaID == "" {
			host, herr := osHostname()
			if herr != nil || host == "" {
				host = "unknown-host"
			}
			replicaID = fmt.Sprintf("%s-%d", host, os.Getpid())
		}
		syncer, serr := initBreakerSync(cpRuntime, hm, replicaID, cfg.BreakerSyncInterval(), logger.Printf)
		if serr != nil {
			fatalLog(bootstrap, "cannot initialize breaker sync: %v", serr)
		}
		go syncer.Run(ctx)
		logger.Printf("breaker_sync enabled replica=%s interval=%s backend=%s", replicaID, cfg.BreakerSyncInterval(), cfg.ControlPlane.Backend)
	}
	listener, err := netListen("tcp", cfg.Listen)
	if err != nil {
		fatalLog(bootstrap, "cannot listen on %s (another instance may be running): %v", cfg.Listen, err)
	}
	defer listener.Close()
	secureListener := cfg.TLS.Enabled
	serverErr := make(chan error, 1)
	var serveListener = listener
	if cfg.TLS.Enabled {
		tlsConfig, tlsErr := transport.ServerTLSConfig(cfg.TLS, filepath.Dir(*configPath))
		if tlsErr != nil {
			fatalLog(bootstrap, "TLS configuration is invalid: %v", tlsErr)
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
		logCloseError(logger, "control_plane_close_error", cpRuntime.Close())
	}
	select {
	case err := <-serverErr:
		fatalLog(logger, "server_error=%v", err)
	default:
	}
}

func dashboardURL(configPath string) string {
	cfg, err := config.Load(configPath)
	if err != nil {
		return "http://127.0.0.1:8080/"
	}
	// config.Load validates Listen, so SplitHostPort cannot fail here.
	host, port, _ := net.SplitHostPort(cfg.Listen)
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

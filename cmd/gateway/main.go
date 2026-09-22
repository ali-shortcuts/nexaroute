package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/httpapi"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

const version = "0.3"

func defaultConfigPath() string {
	if p := os.Getenv("NEXAROUTE_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("config.json"); err == nil {
		return "config.json"
	}
	if _, err := os.Stat(filepath.Join("configs", "config.example.json")); err == nil {
		return filepath.Join("configs", "config.example.json")
	}
	return "config.json"
}

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
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("NexaRoute v" + version)
		return
	}
	logger := log.New(os.Stdout, "nexaroute ", log.LstdFlags|log.Lmicroseconds)
	if err := ensureConfig(*configPath); err != nil {
		logger.Fatal(err)
	}
	if err := config.RemoveStaleBackup(*configPath); err != nil {
		logger.Fatal(err)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatal(err)
	}
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		logger.Fatal(err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(500)
	pe := probe.New(cfg, reg, rt, hm, bus)
	api := httpapi.New(cfg, *configPath, reg, rt, hm, bus, pe, logger)
	srv := &http.Server{Addr: cfg.Listen, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 180 * time.Second, MaxHeaderBytes: 1 << 20}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go pe.Run(ctx)
	go func() {
		logger.Printf("version=%s config=%s listening=http://%s", version, *configPath, cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Printf("server_error=%v", err)
			cancel()
		}
	}()
	<-ctx.Done()
	shutdown, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdown); err != nil {
		logger.Printf("shutdown_error=%v", err)
	}
}

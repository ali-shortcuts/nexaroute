package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/securitystore"
)

// runDoctor implements `nexaroute doctor`: read-only diagnostics for the
// local installation. It prints one PASS/WARN/FAIL line per check and
// exits 0 when nothing failed, 1 when any check failed, and 2 on usage
// errors. It never writes and never prints secret values, DSNs, URLs or
// audit contents — only statuses, counts and permission bits.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath(), "path to JSON config")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	d := &doctor{w: stdout}
	d.checkConfig(*configPath)
	d.checkSecurityStore(*configPath)
	d.checkControlPlane(*configPath)
	fmt.Fprintf(stdout, "doctor summary: %d pass, %d warn, %d fail\n", d.passes, d.warns, d.fails)
	if d.fails > 0 {
		return 1
	}
	return 0
}

type doctor struct {
	w                    io.Writer
	cfg                  *config.Config
	passes, warns, fails int
}

func (d *doctor) report(status, name, detail string) {
	fmt.Fprintf(d.w, "%s %s: %s\n", status, name, detail)
	switch status {
	case "PASS":
		d.passes++
	case "WARN":
		d.warns++
	default:
		d.fails++
	}
}

func (d *doctor) checkConfig(configPath string) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		d.report("FAIL", "config", fmt.Sprintf("cannot read %s; run the gateway once or fix the path/permissions", configPath))
		return
	}
	if info, statErr := os.Stat(configPath); statErr != nil {
		// Never touch info here: a failed Stat returns a nil
		// FileInfo, and dereferencing it panics the whole run.
		d.report("FAIL", "config_permissions", fmt.Sprintf("cannot stat %s (%s)", configPath, fsErrorClass(statErr)))
	} else if info.Mode().Perm()&0o077 != 0 {
		d.report("WARN", "config_permissions", fmt.Sprintf("%s is mode %04o; group/other access is discouraged for a file holding encrypted secrets", configPath, info.Mode().Perm()))
	} else {
		d.report("PASS", "config_permissions", fmt.Sprintf("%s mode %04o", configPath, info.Mode().Perm()))
	}
	cfg, err := config.LoadBase(configPath)
	if err != nil {
		d.report("FAIL", "config", "load/validation failed; run `nexaroute config validate` for details")
		return
	}
	d.cfg = &cfg
	d.report("PASS", "config", fmt.Sprintf("loads and validates (%d providers, %d route profiles)", len(cfg.Providers), len(cfg.RouteProfiles)))
	if len(cfg.Warnings) > 0 {
		d.report("WARN", "config_warnings", fmt.Sprintf("%d advisory warning(s) from defaulted settings", len(cfg.Warnings)))
	}
	if envelopes := bytes.Count(raw, []byte("nxs1:")); envelopes > 0 {
		// LoadBase resolves the master key to decrypt in memory; reaching
		// this point means the key paired with the stored envelopes works.
		d.report("PASS", "master_key", fmt.Sprintf("resolvable for %d encrypted value(s)", envelopes))
	} else {
		d.report("PASS", "master_key", "no encrypted values stored")
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		d.report("FAIL", "listen", fmt.Sprintf("%q does not parse as host:port", cfg.Listen))
	} else {
		d.report("PASS", "listen", cfg.Listen)
	}
}

func (d *doctor) checkSecurityStore(configPath string) {
	if d.cfg == nil {
		d.report("WARN", "security_store", "skipped (config did not load)")
		return
	}
	switch d.cfg.Admin.SecurityStoreBackend {
	case "", "bbolt":
	default:
		d.report("WARN", "security_store", fmt.Sprintf("backend %q adapters land with M4-2; admin authentication fails closed in this build", d.cfg.Admin.SecurityStoreBackend))
		return
	}
	path, err := resolveSecurityStorePath(d.cfg.Admin.SecurityStorePath, configPath)
	if err != nil {
		d.report("FAIL", "security_store", "path cannot be resolved from the configuration")
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		// Only genuine absence is a benign "not created yet".
		// Permission errors, a non-directory path component and any
		// other filesystem failure mean an installation that cannot
		// be inspected — a FAIL, never a reassuring WARN.
		if errors.Is(err, os.ErrNotExist) {
			d.report("WARN", "security_store", "not created yet; it is created with mode 0600 in a 0700 directory on first start")
		} else {
			d.report("FAIL", "security_store", fmt.Sprintf("cannot inspect %s (%s)", path, fsErrorClass(err)))
		}
		return
	}
	if info.Mode().Perm()&0o077 != 0 {
		d.report("WARN", "security_store_permissions", fmt.Sprintf("%s is mode %04o; want 0600", path, info.Mode().Perm()))
	}
	store, err := securitystore.OpenReadOnly(path)
	if err != nil {
		d.report("FAIL", "security_store", "cannot open read-only; the file may be corrupt or locked by an incompatible process")
		return
	}
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	report, err := store.VerifyAuditChain(ctx)
	if err != nil {
		d.report("FAIL", "audit_chain", "verification errored; run `nexaroute audit verify` for details")
		return
	}
	if !report.Verified || len(report.Problems) > 0 {
		d.report("FAIL", "audit_chain", fmt.Sprintf("%d problem(s) across %d record(s); run `nexaroute audit verify` for details", len(report.Problems), report.TotalRecords))
		return
	}
	d.report("PASS", "security_store", fmt.Sprintf("audit chain verified: %d record(s), %d chained", report.TotalRecords, report.ChainedRecords))
}

func (d *doctor) checkControlPlane(configPath string) {
	if d.cfg == nil {
		d.report("WARN", "control_plane", "skipped (config did not load)")
		return
	}
	cp := d.cfg.ControlPlane
	if !cp.Enabled {
		d.report("PASS", "control_plane", "disabled (zero-service default)")
		return
	}
	switch cp.Backend {
	case "", "file":
		path := cp.StatePath
		if path == "" {
			path = configPath + ".controlplane.json"
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(configPath), path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				d.report("WARN", "control_plane", "file backend state not created yet")
			} else {
				d.report("FAIL", "control_plane", fmt.Sprintf("cannot read file backend state %s (%s)", path, fsErrorClass(err)))
			}
			return
		}
		var probe map[string]any
		if err := json.Unmarshal(raw, &probe); err != nil {
			d.report("FAIL", "control_plane", "file backend state is corrupt (not JSON)")
			return
		}
		d.report("PASS", "control_plane", "file backend state present and parseable")
	case "memory":
		d.report("PASS", "control_plane", "memory backend (ephemeral, for tests)")
	case "postgres":
		d.checkExternal("control_plane_postgres", cp.PostgresDSNEnv, func(ctx context.Context, value string) error {
			db, err := sql.Open("pgx", value)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			return db.PingContext(ctx)
		})
	case "redis":
		d.checkExternal("control_plane_redis", cp.RedisURLEnv, func(ctx context.Context, value string) error {
			opts, err := redis.ParseURL(value)
			if err != nil {
				return err
			}
			client := redis.NewClient(opts)
			defer func() { _ = client.Close() }()
			return client.Ping(ctx).Err()
		})
	default:
		d.report("FAIL", "control_plane", fmt.Sprintf("unknown backend %q", cp.Backend))
	}
}

// checkExternal pings an external dependency using only the env var
// named by the configuration; errors are reduced to a failure class so
// no DSN/URL material can leak into diagnostic output.
func (d *doctor) checkExternal(name, envName string, ping func(ctx context.Context, value string) error) {
	if envName == "" {
		d.report("FAIL", name, "no environment variable is configured for the connection value")
		return
	}
	value := os.Getenv(envName)
	if value == "" {
		d.report("FAIL", name, fmt.Sprintf("environment variable %s is not set or is empty", envName))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ping(ctx, value); err != nil {
		d.report("FAIL", name, fmt.Sprintf("unreachable via %s (check network, credentials and service health)", envName))
		return
	}
	d.report("PASS", name, fmt.Sprintf("reachable via %s", envName))
}

// fsErrorClass reduces a filesystem error to a coarse, safe class
// for diagnostic output — no paths, values or wrapped internals, so
// doctor output stays free of anything sensitive while still telling
// the operator WHAT kind of problem to fix.
func fsErrorClass(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "not found"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	case errors.Is(err, syscall.ENOTDIR):
		return "a path component is not a directory"
	case errors.Is(err, syscall.EISDIR):
		return "the path is a directory"
	default:
		return "filesystem error"
	}
}

// resolveSecurityStorePath mirrors the server-side rule in
// internal/httpapi (default security/nexaroute-security.db relative to
// the config directory; configured paths resolve against it too).
func resolveSecurityStorePath(configuredPath, configPath string) (string, error) {
	configuredPath = strings.TrimSpace(configuredPath)
	configPath = strings.TrimSpace(configPath)
	if configuredPath == "" {
		if configPath == "" {
			return "", fmt.Errorf("admin security store path is required when no config file path is available")
		}
		configuredPath = filepath.Join("security", "nexaroute-security.db")
	}
	if filepath.IsAbs(configuredPath) {
		return filepath.Clean(configuredPath), nil
	}
	base := "."
	if configPath != "" {
		base = filepath.Dir(configPath)
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	return filepath.Join(absBase, configuredPath), nil
}

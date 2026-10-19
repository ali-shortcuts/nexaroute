//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	outageOperationTimeout = 5 * time.Second
	outageRecoveryTimeout  = 20 * time.Second
	outageConcurrency      = 32
)

type tcpFaultProxy struct {
	target   string
	listener net.Listener
	outageMu sync.RWMutex
	outage   bool
	connsMu  sync.Mutex
	conns    map[net.Conn]struct{}
}

func newTCPFaultProxy(t *testing.T, target string) *tcpFaultProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start fault proxy listener: %v", err)
	}
	p := &tcpFaultProxy{target: target, listener: ln, conns: make(map[net.Conn]struct{})}
	go p.acceptLoop()
	t.Cleanup(p.Close)
	return p
}

func (p *tcpFaultProxy) Addr() string { return p.listener.Addr().String() }

func (p *tcpFaultProxy) SetOutage(outage bool) {
	p.outageMu.Lock()
	p.outage = outage
	p.outageMu.Unlock()
	if !outage {
		return
	}
	p.connsMu.Lock()
	for conn := range p.conns {
		_ = conn.Close()
	}
	p.connsMu.Unlock()
}

func (p *tcpFaultProxy) Close() {
	p.SetOutage(true)
	_ = p.listener.Close()
}

func (p *tcpFaultProxy) acceptLoop() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.outageMu.RLock()
		outage := p.outage
		p.outageMu.RUnlock()
		if outage {
			_ = client.Close()
			continue
		}
		go p.forward(client)
	}
}

func (p *tcpFaultProxy) forward(client net.Conn) {
	upstream, err := net.DialTimeout("tcp", p.target, 2*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	p.track(client)
	p.track(upstream)
	defer p.untrack(client)
	defer p.untrack(upstream)
	defer client.Close()
	defer upstream.Close()

	p.outageMu.RLock()
	outage := p.outage
	p.outageMu.RUnlock()
	if outage {
		return
	}

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
}

func (p *tcpFaultProxy) track(conn net.Conn) {
	p.connsMu.Lock()
	p.conns[conn] = struct{}{}
	p.connsMu.Unlock()
}

func (p *tcpFaultProxy) untrack(conn net.Conn) {
	p.connsMu.Lock()
	delete(p.conns, conn)
	p.connsMu.Unlock()
}

func proxyTarget(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		t.Fatalf("parse integration URL host: %v", err)
	}
	if _, _, err := net.SplitHostPort(parsed.Host); err != nil {
		t.Fatalf("integration URL must include an explicit host:port: %v", err)
	}
	return parsed.Host
}

func proxiedURL(t *testing.T, rawURL, proxyAddr string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse integration URL: %v", err)
	}
	parsed.Host = proxyAddr
	return parsed.String()
}

func runOutageConcurrency(t *testing.T, operate func(context.Context) error) {
	t.Helper()
	errs := make(chan error, outageConcurrency)
	var wg sync.WaitGroup
	for i := 0; i < outageConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), outageOperationTimeout)
			defer cancel()
			errs <- operate(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("bounded outage load returned an unexpected error: %v", err)
		}
	}
}

func waitForDurable(t *testing.T, manager *Manager, namespace string, wantRevision uint64, wantPayload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), outageRecoveryTimeout)
	defer cancel()
	for {
		result, err := manager.Reconcile(ctx, namespace)
		if err == nil && result.Source == "durable" {
			snapshot, err := manager.Load(ctx, namespace)
			if err != nil {
				t.Fatalf("load recovered snapshot: %v", err)
			}
			if snapshot.Revision != wantRevision || string(snapshot.Payload) != wantPayload {
				t.Fatalf("recovered state=%+v, want revision=%d payload=%q", snapshot, wantRevision, wantPayload)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("real service did not become durable again within %s: result=%+v err=%v", outageRecoveryTimeout, result, err)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func TestPostgresIntegrationOutageRecoveryCancellationBackpressure(t *testing.T) {
	rawDSN := integrationDSN(t)
	proxy := newTCPFaultProxy(t, proxyTarget(t, rawDSN))

	db, err := sql.Open("pgx", proxiedURL(t, rawDSN, proxy.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("database through fault proxy unreachable: %v", err)
	}
	registry, err := DefaultMigrationRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, db, registry); err != nil {
		t.Fatalf("apply migrations through fault proxy: %v", err)
	}
	store, err := NewSQLStore(db)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store, LastKnownGood)
	if err != nil {
		t.Fatal(err)
	}
	ns := uniqueNamespace(t)
	if _, err := manager.Save(ctx, ns, 0, []byte("before-outage")); err != nil {
		t.Fatalf("seed first revision: %v", err)
	}
	seeded, err := manager.Save(ctx, ns, 1, []byte("before-outage-v2"))
	if err != nil {
		t.Fatalf("seed second revision: %v", err)
	}

	proxy.SetOutage(true)

	got, err := manager.Load(ctx, ns)
	if err != nil {
		t.Fatalf("last-known-good load during PostgreSQL outage: %v", err)
	}
	if got.Revision != seeded.Revision || string(got.Payload) != "before-outage-v2" {
		t.Fatalf("last-known-good state=%+v, want revision=%d", got, seeded.Revision)
	}

	canceledCtx, cancelLoad := context.WithCancel(context.Background())
	cancelLoad()
	if _, err := manager.Load(canceledCtx, ns); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled load during outage: %v, want context.Canceled", err)
	}

	runOutageConcurrency(t, func(opCtx context.Context) error {
		snapshot, err := manager.Load(opCtx, ns)
		if err != nil {
			return err
		}
		if snapshot.Revision != seeded.Revision || string(snapshot.Payload) != "before-outage-v2" {
			return fmt.Errorf("concurrent last-known-good state=%+v", snapshot)
		}
		return nil
	})
	if stats := db.Stats(); stats.OpenConnections > stats.MaxOpenConnections {
		t.Fatalf("database pool exceeded its bound: open=%d max=%d", stats.OpenConnections, stats.MaxOpenConnections)
	}

	proxy.SetOutage(false)
	waitForDurable(t, manager, ns, seeded.Revision, "before-outage-v2")

	cont := context.Background()
	next, err := manager.Save(cont, ns, seeded.Revision, []byte("after-recovery"))
	if err != nil {
		t.Fatalf("save after PostgreSQL recovery: %v", err)
	}
	if next.Revision != seeded.Revision+1 {
		t.Fatalf("recovered write revision=%d, want %d", next.Revision, seeded.Revision+1)
	}
	runOutageConcurrency(t, func(opCtx context.Context) error {
		snapshot, err := manager.Load(opCtx, ns)
		if err != nil {
			return err
		}
		if snapshot.Revision != next.Revision || string(snapshot.Payload) != "after-recovery" {
			return fmt.Errorf("post-recovery state=%+v", snapshot)
		}
		return nil
	})
}

func TestRedisIntegrationOutageRecoveryCancellationBackpressure(t *testing.T) {
	rawURL := os.Getenv("NEXAROUTE_TEST_REDIS_URL")
	if rawURL == "" {
		t.Skip("NEXAROUTE_TEST_REDIS_URL not set; skipping real-Redis integration test")
	}
	proxy := newTCPFaultProxy(t, proxyTarget(t, rawURL))

	opts, err := redis.ParseURL(proxiedURL(t, rawURL, proxy.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	opts.PoolSize = 4
	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = 2 * time.Second
	opts.WriteTimeout = 2 * time.Second
	opts.PoolTimeout = 2 * time.Second
	opts.MaxRetries = 0
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis through fault proxy unreachable: %v", err)
	}
	store, err := NewRedisStore(goRedisHashAdapter{client: client})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store, LastKnownGood)
	if err != nil {
		t.Fatal(err)
	}
	ns := uniqueRedisNamespace(t, client)
	if _, err := manager.Save(ctx, ns, 0, []byte("before-outage")); err != nil {
		t.Fatalf("seed first revision: %v", err)
	}
	seeded, err := manager.Save(ctx, ns, 1, []byte("before-outage-v2"))
	if err != nil {
		t.Fatalf("seed second revision: %v", err)
	}

	proxy.SetOutage(true)

	got, err := manager.Load(ctx, ns)
	if err != nil {
		t.Fatalf("last-known-good load during Redis outage: %v", err)
	}
	if got.Revision != seeded.Revision || string(got.Payload) != "before-outage-v2" {
		t.Fatalf("last-known-good state=%+v, want revision=%d", got, seeded.Revision)
	}

	canceledCtx, cancelLoad := context.WithCancel(context.Background())
	cancelLoad()
	if _, err := manager.Load(canceledCtx, ns); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled load during outage: %v, want context.Canceled", err)
	}

	runOutageConcurrency(t, func(opCtx context.Context) error {
		snapshot, err := manager.Load(opCtx, ns)
		if err != nil {
			return err
		}
		if snapshot.Revision != seeded.Revision || string(snapshot.Payload) != "before-outage-v2" {
			return fmt.Errorf("concurrent last-known-good state=%+v", snapshot)
		}
		return nil
	})
	if stats := client.PoolStats(); stats.TotalConns > uint32(opts.PoolSize) {
		t.Fatalf("Redis pool exceeded its bound: total=%d max=%d", stats.TotalConns, opts.PoolSize)
	}

	proxy.SetOutage(false)
	waitForDurable(t, manager, ns, seeded.Revision, "before-outage-v2")

	cont := context.Background()
	next, err := manager.Save(cont, ns, seeded.Revision, []byte("after-recovery"))
	if err != nil {
		t.Fatalf("save after Redis recovery: %v", err)
	}
	if next.Revision != seeded.Revision+1 {
		t.Fatalf("recovered write revision=%d, want %d", next.Revision, seeded.Revision+1)
	}
	runOutageConcurrency(t, func(opCtx context.Context) error {
		snapshot, err := manager.Load(opCtx, ns)
		if err != nil {
			return err
		}
		if snapshot.Revision != next.Revision || string(snapshot.Payload) != "after-recovery" {
			return fmt.Errorf("post-recovery state=%+v", snapshot)
		}
		return nil
	})
}

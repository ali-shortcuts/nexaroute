package egress

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Policy is the provider egress boundary. AllowedHosts is an optional exact
// host allow-list; AllowLoopback permits only explicitly local HTTP providers.
type Policy struct {
	AllowedHosts  []string
	AllowLoopback bool
}

func (p Policy) allowsHost(host string) bool {
	if len(p.AllowedHosts) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, allowed := range p.AllowedHosts {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(allowed), "."), host) {
			return true
		}
	}
	return false
}

func blocked(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	if ip.IsLoopback() {
		return !allowLoopback
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return true
	}
	return false
}

func (p Policy) validateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("egress target is malformed or contains credentials/query")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && p.AllowLoopback) {
		return nil, fmt.Errorf("egress target must use https (http is only allowed for loopback development)")
	}
	if !p.allowsHost(u.Hostname()) {
		return nil, fmt.Errorf("egress host %q is not allow-listed", u.Hostname())
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && blocked(ip, p.AllowLoopback) {
		return nil, fmt.Errorf("egress target %q is blocked", u.Hostname())
	}
	return u, nil
}

// NewTransport returns a direct, pinned transport. Environment proxies are
// intentionally ignored; proxy egress must be an independently controlled
// gateway rather than an implicit process setting.
func (p Policy) NewTransport(target string, proxyURL string) (*http.Transport, error) {
	u, err := p.validateURL(target)
	if err != nil {
		return nil, err
	}
	var proxy func(*http.Request) (*url.URL, error)
	if proxyURL != "" {
		proxyTarget, parseErr := url.Parse(proxyURL)
		if parseErr != nil || proxyTarget.Hostname() == "" || (proxyTarget.Scheme != "http" && proxyTarget.Scheme != "https") {
			return nil, fmt.Errorf("provider proxy_url is malformed")
		}
		// Proxy credentials and URL decorations are never sent to the proxy
		// client from provider configuration; the admin API redacts them and the
		// egress layer reduces the runtime target to scheme plus authority.
		proxyTarget.User = nil
		proxyTarget.Path, proxyTarget.RawPath, proxyTarget.RawQuery, proxyTarget.Fragment = "", "", "", ""
		proxy = http.ProxyURL(proxyTarget)
	}
	resolver := net.DefaultResolver
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if !p.allowsHost(host) {
			return nil, fmt.Errorf("egress host %q is not allow-listed", host)
		}
		if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
			if blocked(ip, p.AllowLoopback) {
				return nil, fmt.Errorf("egress address %q is blocked", host)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		ips, err := resolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("egress DNS resolution failed for %q: %w", host, err)
		}
		for _, candidate := range ips {
			ip, ok := netip.AddrFromSlice(candidate.IP)
			if !ok || blocked(ip, p.AllowLoopback) {
				return nil, fmt.Errorf("egress DNS answer for %q is blocked", host)
			}
		}
		var last error
		for _, candidate := range ips {
			ip := netip.MustParseAddr(candidate.IP.String())
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			} else {
				last = dialErr
			}
		}
		return nil, last
	}
	return &http.Transport{Proxy: proxy, DialContext: dial, ForceAttemptHTTP2: true, MaxIdleConns: 64, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 60 * time.Second, ExpectContinueTimeout: time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()}}, nil
}

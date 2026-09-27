package remote

// Deterministic test doubles for DNS and internal-target behavior.
//
// These helpers let tests simulate DNS rebinding (different answers per
// lookup), mixed public/private answers, resolver failures and "internal
// service" endpoints without any dependency on the public internet or on
// /etc/hosts.

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDNSServer is a minimal DNS responder that replays a scripted, ordered
// list of A records per name: every A query for a name consumes the next
// answer. AAAA queries always return NODATA; unscripted names get NXDOMAIN.
// This makes DNS rebinding fully deterministic: lookup #1 and lookup #2 can
// return different IPs.
type fakeDNSServer struct {
	conn net.PacketConn

	mu       sync.Mutex
	seq      map[string][]net.IP
	aQueries map[string]int
}

func newFakeDNSServer(t *testing.T) *fakeDNSServer {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake DNS: listen udp: %v", err)
	}
	s := &fakeDNSServer{
		conn:     pc,
		seq:      map[string][]net.IP{},
		aQueries: map[string]int{},
	}
	go s.serve()
	t.Cleanup(func() { _ = s.conn.Close() })
	return s
}

func normalizeDNSName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// setA queues the ordered A answers returned for name (one per A query).
func (s *fakeDNSServer) setA(name string, ips ...net.IP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := normalizeDNSName(name)
	s.seq[key] = append(s.seq[key], ips...)
}

func (s *fakeDNSServer) addr() string { return s.conn.LocalAddr().String() }

// aQueryCount returns how many A queries were received for name.
func (s *fakeDNSServer) aQueryCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aQueries[normalizeDNSName(name)]
}

func (s *fakeDNSServer) serve() {
	buf := make([]byte, 1024)
	for {
		n, raddr, err := s.conn.ReadFrom(buf)
		if err != nil {
			return // listener closed
		}
		if resp := s.handle(buf[:n]); resp != nil {
			_, _ = s.conn.WriteTo(resp, raddr)
		}
	}
}

func (s *fakeDNSServer) handle(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	if binary.BigEndian.Uint16(q[4:6]) != 1 { // QDCOUNT
		return nil
	}
	name, qtype, qEnd, ok := parseDNSQuestion(q[12:])
	if !ok {
		return nil
	}
	name = normalizeDNSName(name)

	var answers []net.IP
	rcode := 0
	switch qtype {
	case 1: // A
		s.mu.Lock()
		s.aQueries[name]++
		var ip net.IP
		if ips := s.seq[name]; len(ips) > 0 {
			ip = ips[0]
			s.seq[name] = ips[1:]
		}
		s.mu.Unlock()
		switch {
		case ip == nil:
			rcode = 3 // NXDOMAIN for unscripted names
		case ip.To4() != nil:
			answers = append(answers, ip.To4())
		default:
			// scripted answer is not IPv4: NODATA
		}
	case 28: // AAAA: NODATA (tests script IPv4 answers only)
	default:
		return nil
	}
	// qEnd is relative to q[12:]; the response echoes the raw question.
	return buildDNSResponse(q, q[12:12+qEnd], answers, rcode)
}

// parseDNSQuestion parses an uncompressed DNS question section.
func parseDNSQuestion(b []byte) (name string, qtype uint16, qEnd int, ok bool) {
	off := 0
	var labels []string
	for {
		if off >= len(b) {
			return "", 0, 0, false
		}
		n := int(b[off])
		if n == 0 {
			off++
			break
		}
		if n&0xC0 != 0 {
			return "", 0, 0, false // compression not expected in questions
		}
		off++
		if off+n > len(b) {
			return "", 0, 0, false
		}
		labels = append(labels, string(b[off:off+n]))
		off += n
	}
	if off+4 > len(b) {
		return "", 0, 0, false
	}
	qtype = binary.BigEndian.Uint16(b[off : off+2])
	qEnd = off + 4 // qtype + qclass
	return strings.Join(labels, "."), qtype, qEnd, true
}

func buildDNSResponse(q []byte, question []byte, answers []net.IP, rcode int) []byte {
	out := make([]byte, 0, len(question)+16*len(answers))
	out = append(out, q[0], q[1]) // ID
	flags := uint16(0x8180) | uint16(rcode&0xF)
	out = append(out, byte(flags>>8), byte(flags))
	out = append(out, 0, 1) // QDCOUNT
	out = append(out, byte(len(answers)>>8), byte(len(answers)))
	out = append(out, 0, 0) // NSCOUNT
	out = append(out, 0, 0) // ARCOUNT
	out = append(out, question...)
	for _, ip := range answers {
		ip4 := ip.To4()
		out = append(out, 0xC0, 0x0C) // NAME: pointer to question at offset 12
		out = append(out, 0, 1)       // TYPE A
		out = append(out, 0, 1)       // CLASS IN
		out = append(out, 0, 0, 0, 1) // TTL
		out = append(out, 0, 4)       // RDLENGTH
		out = append(out, ip4[0], ip4[1], ip4[2], ip4[3])
	}
	return out
}

// fakeResolver serves scripted DNS lookups without a real resolver, and counts
// the calls. Each add() call scripts the complete answer for one lookup; every
// lookup consumes the next scripted batch (rebinding simulation).
type fakeResolver struct {
	mu    sync.Mutex
	calls int
	hosts []string
	seq   map[string][][]net.IPAddr
	err   error // if set, every lookup fails with this error
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{seq: map[string][][]net.IPAddr{}}
}

// add scripts the answer batch for one lookup of host.
func (r *fakeResolver) add(host string, ips ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	batch := make([]net.IPAddr, 0, len(ips))
	for _, s := range ips {
		batch = append(batch, net.IPAddr{IP: net.ParseIP(s)})
	}
	key := normalizeDNSName(host)
	r.seq[key] = append(r.seq[key], batch)
}

func (r *fakeResolver) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *fakeResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// hostsSeen returns every hostname passed to the resolver, in order.
func (r *fakeResolver) hostsSeen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.hosts))
	copy(out, r.hosts)
	return out
}

func (r *fakeResolver) lookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.hosts = append(r.hosts, host)
	if r.err != nil {
		return nil, r.err
	}
	key := normalizeDNSName(host)
	if len(r.seq[key]) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	// Consume one scripted batch per call (rebinding simulation).
	out := r.seq[key][0]
	r.seq[key] = r.seq[key][1:]
	return out, nil
}

// canaryListener stands in for an "internal service" (bound to loopback) and
// counts accepted connections, so tests can prove a connection never reached a
// blocked target.
type canaryListener struct {
	ln       net.Listener
	accepted atomic.Int64
}

func startCanary(t *testing.T) *canaryListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("canary: listen: %v", err)
	}
	c := &canaryListener{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.accepted.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return c
}

func (c *canaryListener) port() int {
	return c.ln.Addr().(*net.TCPAddr).Port
}

func (c *canaryListener) hits() int64 { return c.accepted.Load() }

// ---- dial fakes ----

type dialAttempt struct {
	network string
	addr    string
}

// recordDialer records every dial target and returns stub connections (or a
// configured error/hook). It is the seam that proves the production dialer
// only ever receives approved IP literals.
type recordDialer struct {
	mu       sync.Mutex
	attempts []dialAttempt
	hook     func(network, addr string) (net.Conn, error) // optional routing
	err      error                                        // used when hook is nil
}

func (d *recordDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.attempts = append(d.attempts, dialAttempt{network: network, addr: addr})
	d.mu.Unlock()
	if d.hook != nil {
		return d.hook(network, addr)
	}
	if d.err != nil {
		return nil, d.err
	}
	return stubConn{}, nil
}

func (d *recordDialer) addrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.attempts))
	for _, a := range d.attempts {
		out = append(out, a.addr)
	}
	return out
}

func (d *recordDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.attempts)
}

// stubConn is a net.Conn that is never used for I/O (dial-policy unit tests).
type stubConn struct{}

func (stubConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (stubConn) Write(b []byte) (int, error)      { return len(b), nil }
func (stubConn) Close() error                     { return nil }
func (stubConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (stubConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (stubConn) SetDeadline(time.Time) error      { return nil }
func (stubConn) SetReadDeadline(time.Time) error  { return nil }
func (stubConn) SetWriteDeadline(time.Time) error { return nil }

// Package proxy implements a small HTTP CONNECT proxy that enforces a
// hostname allowlist on outbound HTTPS requests from the agent
// subprocess. agentbox starts this proxy on a random localhost port,
// sets HTTP_PROXY/HTTPS_PROXY env vars on the agent + npm subprocesses,
// and so any HTTP-client library that respects standard proxy env vars
// (most modern SDKs and CLIs do) routes through here.
//
// Threat model: catches well-behaved HTTP traffic. An agent that opens
// raw sockets bypassing HTTP_PROXY would not be caught — that's defense
// in depth at the Docker network layer (cloud-metadata block via
// ExtraHosts; future iptables work in Phase 5.4 v2). The proxy does
// what proxies do well: reject CONNECT requests for non-allowlisted
// hosts.
//
// Hardening layers within the proxy itself:
//   - hostname allowlist (case-insensitive, trailing-dot normalized)
//   - hard deny-list for hostnames that should never be allowlisted
//     (localhost, loopback IPs) regardless of allowlist contents
//   - reject CONNECTs whose host is an IP literal — forces DNS, which
//     we then validate
//   - resolve hostname, reject if any resolved IP is in a
//     private/special range (RFC 1918, 169.254/16, ULA, loopback,
//     multicast, …); dial the resolved IP literal, not the hostname,
//     to defeat DNS rebinding between resolution and dial
//   - concurrency cap (semaphore) bounds in-flight tunnels
//   - read deadline on the CONNECT handshake bounds slowloris exposure
//
// Scope: HTTPS via CONNECT only (port 443). Plain HTTP forward proxying
// is intentionally rejected — modern HTTPS adoption makes this a
// reasonable simplification, and HTTP-only registries / APIs are rare
// enough that opting them in via ADDITIONAL_ALLOWED_HOSTS is the wrong
// answer (they should switch to HTTPS).
package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// hardDenyHosts is a static deny-list that overrides the AllowList.
// Defends against a fat-fingered ADDITIONAL_ALLOWED_HOSTS that names
// loopback or wildcard addresses — these never represent legitimate
// outbound destinations for the agent, so blocking them unconditionally
// is safer than trusting the allowlist contents.
var hardDenyHosts = map[string]struct{}{
	"localhost": {},
	"127.0.0.1": {},
	"0.0.0.0":   {},
	"::1":       {},
	"[::1]":     {},
}

// AllowList is a set of allowed hostnames. Hostnames are matched exact
// (case-insensitive, trailing dot stripped); no wildcard / subdomain
// matching in v1.
//
// Immutable after construction — built once by NewAllowList, then read
// concurrently by per-request goroutines. The goroutine-spawn
// happens-before edge makes the lock-free reads safe. If a runtime
// reload feature is ever added, add synchronization at that point.
type AllowList struct {
	hosts map[string]struct{}
}

// NewAllowList builds an AllowList from a slice. Duplicates are
// silently deduplicated; empty/whitespace-only entries are dropped.
// Entries are normalized the same way Allows normalizes lookups
// (lowercased, trimmed, trailing dot stripped).
func NewAllowList(hosts []string) *AllowList {
	a := &AllowList{hosts: make(map[string]struct{}, len(hosts))}
	for _, h := range hosts {
		h = normalizeHost(h)
		if h == "" {
			continue
		}
		a.hosts[h] = struct{}{}
	}
	return a
}

// Allows reports whether the given host is in the allowlist. Hard-deny
// hosts (loopback, etc.) are rejected regardless of allowlist contents.
func (a *AllowList) Allows(host string) bool {
	host = normalizeHost(host)
	if _, denied := hardDenyHosts[host]; denied {
		return false
	}
	_, ok := a.hosts[host]
	return ok
}

// normalizeHost lowercases, trims whitespace, and strips a single
// trailing dot. The trailing-dot trim handles fully-qualified domain
// names like "api.anthropic.com." (DNS root indicator) consistently.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	return strings.TrimSuffix(h, ".")
}

// ipResolver is the minimal subset of net.Resolver the proxy uses.
// Defined here so tests can inject deterministic resolutions without
// hitting DNS.
type ipResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Config configures Start. Zero values pick safe defaults.
type Config struct {
	// Logger receives allow/deny decisions. nil is allowed — silent.
	Logger io.Writer

	// BlockPrivateIPs, when true, rejects CONNECTs whose hostname
	// resolves to a private/loopback/link-local IP. Caller should set
	// this explicitly; the wrapper in cmd/agentbox/main.go reads
	// AGENTBOX_BLOCK_PRIVATE_IPS (default true).
	BlockPrivateIPs bool

	// MaxConcurrent caps in-flight tunnels. 0 picks DefaultMaxConcurrent.
	MaxConcurrent int

	// ConnectTimeout bounds how long a client has to send the CONNECT
	// request line + headers. 0 picks DefaultConnectTimeout. Slowloris
	// defense — a silent client occupies a concurrency slot for at most
	// this long.
	ConnectTimeout time.Duration

	// ResolveTimeout bounds DNS resolution per CONNECT. 0 picks
	// DefaultResolveTimeout.
	ResolveTimeout time.Duration

	// DialTimeout bounds the upstream TCP connect. 0 picks
	// DefaultDialTimeout. A bounded dial keeps a slot from being held
	// for OS-level TCP retry windows (10s+ on unreachable destinations).
	DialTimeout time.Duration

	// resolver is unexported and only set by tests. Production callers
	// get net.DefaultResolver via Start.
	resolver ipResolver
}

const (
	DefaultMaxConcurrent  = 100
	DefaultConnectTimeout = 10 * time.Second
	DefaultResolveTimeout = 5 * time.Second
	DefaultDialTimeout    = 10 * time.Second
)

// Server is the running CONNECT proxy. Close it when the agent
// subprocess exits so the listener releases.
type Server struct {
	listener        net.Listener
	allow           *AllowList
	logger          io.Writer
	blockPrivateIPs bool
	connectTimeout  time.Duration
	resolveTimeout  time.Duration
	dialTimeout     time.Duration
	resolver        ipResolver
	sem             chan struct{} // semaphore for in-flight tunnels

	// deniedHosts is the dedup set of hostnames the proxy refused
	// because they weren't on the allowlist. Tracked so the agent run
	// can write an actionable list into /result.json — the user then
	// knows which hosts to add to ADDITIONAL_ALLOWED_HOSTS without
	// digging through stderr. Other deny categories (IP literal,
	// non-443 port, non-CONNECT method, private IP) are deliberately
	// NOT tracked here: those represent agent bugs / security-gate
	// violations rather than allowlist gaps, so surfacing them as
	// "add to allowlist" suggestions would be misleading. They still
	// appear in the proxy log for debugging.
	deniedMu    sync.Mutex
	deniedHosts map[string]struct{}
}

// Start binds a listener on 127.0.0.1:<random-port> and serves the
// proxy in a background goroutine.
func Start(allow *AllowList, cfg Config) (*Server, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("proxy listen failed: %w", err)
	}
	maxConc := cfg.MaxConcurrent
	if maxConc <= 0 {
		maxConc = DefaultMaxConcurrent
	}
	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = DefaultConnectTimeout
	}
	resolveTimeout := cfg.ResolveTimeout
	if resolveTimeout <= 0 {
		resolveTimeout = DefaultResolveTimeout
	}
	dialTimeout := cfg.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = DefaultDialTimeout
	}
	resolver := cfg.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	s := &Server{
		listener:        l,
		allow:           allow,
		logger:          cfg.Logger,
		blockPrivateIPs: cfg.BlockPrivateIPs,
		connectTimeout:  connectTimeout,
		resolveTimeout:  resolveTimeout,
		dialTimeout:     dialTimeout,
		resolver:        resolver,
		sem:             make(chan struct{}, maxConc),
		deniedHosts:     make(map[string]struct{}),
	}
	go s.serve()
	return s, nil
}

// Addr returns the host:port the proxy is bound to. Use this to
// construct the HTTP_PROXY env var: "http://" + s.Addr().
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

// Close stops accepting new connections. In-flight tunnels close
// when their underlying io.Copy returns (typically when one side EOFs).
func (s *Server) Close() error {
	return s.listener.Close()
}

func (s *Server) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed; normal shutdown path
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(client net.Conn) {
	defer client.Close()

	// Concurrency cap: try to acquire a slot non-blocking. If full,
	// reject with 503 — better to fail fast than queue clients up.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.deny(client, http.StatusServiceUnavailable, "agentbox proxy: at capacity", "at-capacity")
		return
	}

	// Slowloris defense: bound how long the client gets to send the
	// CONNECT request line + headers. The deadline is cleared once the
	// request is parsed — a tunneled HTTPS conn can legitimately stall
	// mid-stream and we don't want to break it.
	_ = client.SetReadDeadline(time.Now().Add(s.connectTimeout))

	req, err := http.ReadRequest(bufio.NewReader(client))
	if err != nil {
		return
	}
	_ = client.SetReadDeadline(time.Time{})

	if req.Method != http.MethodConnect {
		s.deny(client, http.StatusForbidden, fmt.Sprintf("agentbox proxy: only CONNECT method supported, got %s", req.Method), "non-connect-method")
		return
	}
	host, port, err := net.SplitHostPort(req.URL.Host)
	if err != nil {
		s.deny(client, http.StatusBadRequest, "agentbox proxy: invalid host:port", "invalid-host")
		return
	}
	if port != "443" {
		s.deny(client, http.StatusForbidden, fmt.Sprintf("agentbox proxy: only port 443 allowed, got %s", port), fmt.Sprintf("non-443-port:%s", host))
		return
	}
	// IP-literal CONNECTs are an SSRF/metadata-IP attack pattern. No
	// legitimate HTTP client uses literal IPs in a CONNECT — they use
	// hostnames, which we then resolve and validate. Closes the
	// "CONNECT 169.254.169.254:443" path even when DNS resolution
	// can't help us.
	if net.ParseIP(host) != nil {
		s.deny(client, http.StatusForbidden, fmt.Sprintf("agentbox proxy: IP-literal CONNECT not allowed, got %s", host), fmt.Sprintf("ip-literal:%s", host))
		return
	}
	if !s.allow.Allows(host) {
		s.recordAllowlistDeny(host)
		s.deny(client, http.StatusForbidden, fmt.Sprintf("agentbox proxy: host %q not in allowlist", host), fmt.Sprintf("denied:%s", host))
		return
	}
	s.tunnel(client, host, port)
}

// tunnel resolves the CONNECT target, validates the resolved IP against
// the private-IP deny-list (if enabled), and dials the IP literal — not
// the hostname. Dialing the IP defeats DNS rebinding: an attacker who
// returns a public IP at validation time and a private IP at dial time
// can't switch us, because we never re-resolve.
func (s *Server) tunnel(client net.Conn, host, port string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.resolveTimeout)
	defer cancel()
	addrs, err := s.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		s.deny(client, http.StatusBadGateway, fmt.Sprintf("agentbox proxy: resolve %s failed: %v", host, err), fmt.Sprintf("resolve-failed:%s", host))
		return
	}
	dialIP := s.pickDialIP(addrs)
	if dialIP == nil {
		s.deny(client, http.StatusForbidden, fmt.Sprintf("agentbox proxy: %s resolves only to private/special IPs", host), fmt.Sprintf("private-ip:%s", host))
		return
	}
	dialer := &net.Dialer{Timeout: s.dialTimeout}
	target, err := dialer.Dial("tcp", net.JoinHostPort(dialIP.String(), port))
	if err != nil {
		s.deny(client, http.StatusBadGateway, fmt.Sprintf("agentbox proxy: dial %s failed: %v", host, err), fmt.Sprintf("dial-failed:%s", host))
		return
	}
	defer target.Close()
	if _, err := client.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		return
	}
	// Pipe bytes both ways until one side closes. The connection is
	// opaque TLS to us (we never see plaintext); we just shuttle.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(target, client) }()
	go func() { defer wg.Done(); _, _ = io.Copy(client, target) }()
	wg.Wait()
}

// pickDialIP returns the first resolved IP that is not on the
// private-IP deny-list, or nil if blockPrivateIPs is set and every
// resolved IP is denied. When blockPrivateIPs is false, returns the
// first IP unconditionally.
func (s *Server) pickDialIP(addrs []net.IPAddr) net.IP {
	for _, a := range addrs {
		if a.IP == nil {
			continue
		}
		if s.blockPrivateIPs && isPrivateOrSpecialIP(a.IP) {
			continue
		}
		return a.IP
	}
	return nil
}

// isPrivateOrSpecialIP reports whether ip is in any range that
// shouldn't be reachable from the agent: loopback, link-local
// (including AWS/Azure cloud metadata 169.254.169.254), RFC 1918
// private, ULA, multicast, unspecified, CGN (RFC 6598), and class-E
// reserved. Public DNS resolving an allowlisted hostname to one of
// these is the SSRF pattern this guard exists to close.
func isPrivateOrSpecialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() ||
		ip.IsPrivate() {
		// IsPrivate covers RFC 1918 (v4) and ULA (v6).
		// IsLinkLocalUnicast covers 169.254/16 (v4) and fe80::/10 (v6).
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// CGN 100.64.0.0/10 (RFC 6598). Not a stdlib helper.
		if v4[0] == 100 && (v4[1]&0xc0) == 64 {
			return true
		}
		// 240.0.0.0/4 reserved (former class E).
		if v4[0] >= 240 {
			return true
		}
	}
	return false
}

// recordAllowlistDeny adds host to the dedup set of allowlist-denied
// hostnames. Hostnames are normalized (lowercased, trimmed, trailing
// dot stripped) so the same host requested via different casing /
// fully-qualified-form variants collapses to one entry.
func (s *Server) recordAllowlistDeny(host string) {
	host = normalizeHost(host)
	if host == "" {
		return
	}
	s.deniedMu.Lock()
	defer s.deniedMu.Unlock()
	s.deniedHosts[host] = struct{}{}
}

// DeniedHosts returns a sorted snapshot of hostnames the proxy has
// refused due to allowlist mismatches over the lifetime of the
// Server. Sorted output is for deterministic logging / JSON
// serialization across runs.
func (s *Server) DeniedHosts() []string {
	s.deniedMu.Lock()
	defer s.deniedMu.Unlock()
	out := make([]string, 0, len(s.deniedHosts))
	for h := range s.deniedHosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func (s *Server) deny(client net.Conn, status int, msg, logTag string) {
	body := msg + "\n"
	resp := fmt.Sprintf(
		"HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body,
	)
	_, _ = client.Write([]byte(resp))
	if s.logger != nil {
		fmt.Fprintf(s.logger, "[agentbox proxy] %s: %s\n", logTag, msg)
	}
}

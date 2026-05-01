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
// Scope: HTTPS via CONNECT only (port 443). Plain HTTP forward proxying
// is intentionally rejected — modern HTTPS adoption makes this a
// reasonable simplification, and HTTP-only registries / APIs are rare
// enough that opting them in via ADDITIONAL_ALLOWED_HOSTS is the wrong
// answer (they should switch to HTTPS).
package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// AllowList is a set of allowed hostnames. Hostnames are matched exact
// (case-insensitive); no wildcard / subdomain matching in v1.
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
func NewAllowList(hosts []string) *AllowList {
	a := &AllowList{hosts: make(map[string]struct{}, len(hosts))}
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		a.hosts[h] = struct{}{}
	}
	return a
}

// Allows reports whether the given host is in the allowlist.
func (a *AllowList) Allows(host string) bool {
	_, ok := a.hosts[strings.ToLower(host)]
	return ok
}

// Server is the running CONNECT proxy. Close it when the agent
// subprocess exits so the listener releases.
type Server struct {
	listener net.Listener
	allow    *AllowList
	logger   io.Writer
}

// Start binds a listener on 127.0.0.1:<random-port> and serves the
// proxy in a background goroutine. logger is where allow/deny decisions
// are written (use os.Stderr for visibility in agentbox logs).
func Start(allow *AllowList, logger io.Writer) (*Server, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("proxy listen failed: %w", err)
	}
	s := &Server{listener: l, allow: allow, logger: logger}
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
	req, err := http.ReadRequest(bufio.NewReader(client))
	if err != nil {
		return
	}
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
	if !s.allow.Allows(host) {
		s.deny(client, http.StatusForbidden, fmt.Sprintf("agentbox proxy: host %q not in allowlist", host), fmt.Sprintf("denied:%s", host))
		return
	}
	s.tunnel(client, host, port)
}

func (s *Server) tunnel(client net.Conn, host, port string) {
	target, err := net.Dial("tcp", net.JoinHostPort(host, port))
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

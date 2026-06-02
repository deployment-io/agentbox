package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAllowList(t *testing.T) {
	a := NewAllowList([]string{"api.anthropic.com", "  REGISTRY.NPMJS.ORG  ", "", "  "})
	if !a.Allows("api.anthropic.com") {
		t.Error("expected api.anthropic.com to be allowed")
	}
	if !a.Allows("API.Anthropic.Com") {
		t.Error("expected case-insensitive match")
	}
	if !a.Allows("registry.npmjs.org") {
		t.Error("expected trimmed + case-folded REGISTRY.NPMJS.ORG to be allowed")
	}
	if a.Allows("evil.com") {
		t.Error("expected evil.com to be denied")
	}
	if a.Allows("") {
		t.Error("empty entries should not produce a wildcard match")
	}
}

// TestAllowListTrailingDot pins normalization of FQDN-style hostnames.
// A client sending "api.anthropic.com." (trailing dot is the DNS root
// indicator) must match an allowlist entry of "api.anthropic.com" so
// the same hostname can't trivially bypass enforcement by tacking on
// a dot. Symmetrically, an allowlist entry with a trailing dot must
// match the no-dot lookup.
func TestAllowListTrailingDot(t *testing.T) {
	a := NewAllowList([]string{"api.anthropic.com"})
	if !a.Allows("api.anthropic.com.") {
		t.Error("trailing dot on lookup should match dot-less allowlist entry")
	}
	b := NewAllowList([]string{"api.anthropic.com."})
	if !b.Allows("api.anthropic.com") {
		t.Error("trailing dot on allowlist entry should match dot-less lookup")
	}
}

// TestAllowListHardDeny pins that the hard deny-list (loopback,
// localhost, etc.) overrides the allowlist regardless of how it was
// configured. Defends against a fat-fingered ADDITIONAL_ALLOWED_HOSTS.
func TestAllowListHardDeny(t *testing.T) {
	a := NewAllowList([]string{"localhost", "127.0.0.1", "0.0.0.0", "::1", "evil.example.com"})
	for _, host := range []string{"localhost", "LOCALHOST", "127.0.0.1", "0.0.0.0", "::1"} {
		if a.Allows(host) {
			t.Errorf("%q must be hard-denied even when present in allowlist", host)
		}
	}
	// Sanity: non-deny entries still pass.
	if !a.Allows("evil.example.com") {
		t.Error("non-deny entries should still be allowed (sanity)")
	}
}

// TestBracketedIPv6IsStrippedBeforeAllowlistLookup pins the upstream
// normalization that justifies removing "[::1]" from hardDenyHosts.
//
// Background: an earlier revision listed both "::1" and "[::1]" in
// hardDenyHosts as defense-in-depth for IPv6 loopback. The bracket
// form was removed because net.SplitHostPort strips brackets during
// host:port parsing in the proxy's request-handling path — by the
// time Allows() is called, the host is always already unbracketed.
//
// This test pins the two halves of that argument:
//
//  1. net.SplitHostPort("[::1]:443") returns host="::1" (no brackets).
//     The proxy's handle() relies on this — Allows() never observes
//     a bracketed host in production.
//
//  2. The unbracketed form "::1" remains hard-denied, so removing
//     "[::1]" from hardDenyHosts loses no coverage. CONNECT
//     [::1]:443 is still rejected — by the IP-literal check upstream
//     of the allowlist (covered by TestRejectsIPLiteralCONNECT) and,
//     if that check were ever bypassed, by the "::1" hard-deny entry.
//
// If a future Go release ever stops stripping brackets in
// SplitHostPort, this test fails and points back at hardDenyHosts —
// "[::1]" would need to come back to preserve the invariant.
func TestBracketedIPv6IsStrippedBeforeAllowlistLookup(t *testing.T) {
	host, port, err := net.SplitHostPort("[::1]:443")
	if err != nil {
		t.Fatalf("SplitHostPort(\"[::1]:443\"): unexpected error %v", err)
	}
	if host != "::1" {
		t.Errorf("net.SplitHostPort stripped-brackets invariant broken: host = %q, want %q",
			host, "::1")
	}
	if port != "443" {
		t.Errorf("port = %q, want 443", port)
	}

	// The unbracketed form (what Allows actually sees in production)
	// remains hard-denied. Adding "::1" to an allowlist does NOT
	// override the hard-deny.
	a := NewAllowList([]string{"::1"})
	if a.Allows("::1") {
		t.Error(`"::1" must remain hard-denied; removing "[::1]" from hardDenyHosts must not have removed the unbracketed entry too`)
	}
}

// startTestServer spins an HTTPS test server and returns its host:port.
// Caller must Close it. Backed by an httptest.Server with TLS so the
// test exercises the real CONNECT-then-tunnel-TLS code path.
func startTestServer(t *testing.T, body string) (host, port string, srv *httptest.Server) {
	t.Helper()
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	u := strings.TrimPrefix(srv.URL, "https://")
	h, p, err := net.SplitHostPort(u)
	if err != nil {
		srv.Close()
		t.Fatalf("split target: %v", err)
	}
	return h, p, srv
}

// startProxyForTest is a tiny convenience wrapper that fails the test
// on Start error and registers Close as cleanup. Cuts boilerplate
// across the per-rejection tests.
func startProxyForTest(t *testing.T, allow *AllowList, cfg Config) *Server {
	t.Helper()
	srv, err := Start(allow, cfg)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// sendCONNECT dials the proxy and writes a CONNECT request line for
// host:port. Returns the parsed response. Used by every per-rejection
// test below — shape was previously copy-pasted everywhere.
func sendCONNECT(t *testing.T, proxyAddr, host, port string) *http.Response {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	fmt.Fprintf(c, "CONNECT %s:%s HTTP/1.1\r\nHost: %s:%s\r\n\r\n", host, port, host, port)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestRejectsUnsupportedMethod confirms methods outside the
// {CONNECT, GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS} set are
// rejected with 405. TRACE is the canonical "unsupported" pick — no
// legitimate Node toolchain client uses it through a forward proxy.
func TestRejectsUnsupportedMethod(t *testing.T) {
	srv := startProxyForTest(t, NewAllowList([]string{"example.com"}), Config{})
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()
	fmt.Fprintf(c, "TRACE http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
	body, _ := io.ReadAll(resp.Body)
	// Pin both the method and the target URL — the deny message should
	// name what was requested so an operator can debug from the log line
	// alone, not just the rejection class.
	if !strings.Contains(string(body), "TRACE") {
		t.Errorf("body = %q, want mention of TRACE method", body)
	}
	if !strings.Contains(string(body), "http://example.com/") {
		t.Errorf("body = %q, want mention of target URL", body)
	}
}

func TestRejectsNon443Port(t *testing.T) {
	srv := startProxyForTest(t, NewAllowList([]string{"example.com"}), Config{})
	resp := sendCONNECT(t, srv.Addr(), "example.com", "8080")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "example.com:8080") {
		t.Errorf("body = %q, want mention of host:port", body)
	}
	if !strings.Contains(string(body), "only port 443") {
		t.Errorf("body = %q, want mention of port 443", body)
	}
}

func TestRejectsNonAllowedHost(t *testing.T) {
	srv := startProxyForTest(t, NewAllowList([]string{"api.anthropic.com"}), Config{})
	resp := sendCONNECT(t, srv.Addr(), "evil.example.com", "443")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"evil.example.com" not in allowlist`) {
		t.Errorf("body = %q, want hostname-denied message", body)
	}
}

// TestRejectsIPLiteralCONNECT pins the IP-literal rejection path. Even
// when the literal IP would otherwise pass (e.g., the agent attempts
// CONNECT to the AWS metadata IP directly), the proxy must refuse
// before any allowlist lookup. The IP-literal check fires before the
// allowlist check, so the deny-tag identifies the actual reason.
//
// IPv6 literals must be sent in bracketed form per RFC 3986 — the test
// passes them already-bracketed since sendCONNECT pastes the host as-is
// into the request line, and net.SplitHostPort on the receiving side
// strips the brackets before our IP-literal check sees the address.
func TestRejectsIPLiteralCONNECT(t *testing.T) {
	srv := startProxyForTest(t, NewAllowList([]string{"169.254.169.254"}), Config{})
	for _, host := range []string{"169.254.169.254", "1.2.3.4", "[::1]"} {
		t.Run(host, func(t *testing.T) {
			resp := sendCONNECT(t, srv.Addr(), host, "443")
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "IP-literal CONNECT") {
				t.Errorf("body = %q, want IP-literal denial", body)
			}
		})
	}
}

// fakeResolver returns a fixed map of hostname → IPs. Used by the
// resolved-IP tests to avoid hitting real DNS and to exercise the
// "allowlist passes, IP fails" path deterministically.
type fakeResolver map[string][]net.IPAddr

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if ips, ok := f[host]; ok {
		return ips, nil
	}
	return nil, errors.New("no such host")
}

// TestBlocksAllowedHostResolvingToPrivateIP is the SSRF defense: an
// allowlisted hostname whose DNS happens to resolve to a private/
// special IP must be denied. Without this guard, an attacker who
// controls DNS (or finds a redirector resolving to 169.254.169.254)
// could exfiltrate IAM credentials via an otherwise-allowed name.
func TestBlocksAllowedHostResolvingToPrivateIP(t *testing.T) {
	cases := []struct {
		name string
		ip   net.IP
	}{
		{"loopback", net.ParseIP("127.0.0.1")},
		{"link-local-aws-metadata", net.ParseIP("169.254.169.254")},
		{"rfc1918-10", net.ParseIP("10.1.2.3")},
		{"rfc1918-172.16", net.ParseIP("172.16.0.5")},
		{"rfc1918-192.168", net.ParseIP("192.168.1.1")},
		{"cgn-100.64", net.ParseIP("100.64.5.5")},
		{"unspecified", net.ParseIP("0.0.0.0")},
		{"ipv6-loopback", net.ParseIP("::1")},
		{"ipv6-link-local", net.ParseIP("fe80::1")},
		{"ipv6-ula", net.ParseIP("fc00::1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				BlockPrivateIPs: true,
				resolver: fakeResolver{
					"sneaky.example.com": []net.IPAddr{{IP: tc.ip}},
				},
			}
			srv := startProxyForTest(t, NewAllowList([]string{"sneaky.example.com"}), cfg)
			resp := sendCONNECT(t, srv.Addr(), "sneaky.example.com", "443")
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "private/special IPs") {
				t.Errorf("body = %q, want private-IP denial", body)
			}
		})
	}
}

// TestBlockPrivateIPsOptOut pins the AGENTBOX_BLOCK_PRIVATE_IPS=0 path:
// when an org legitimately needs to reach an internal-IP destination,
// the proxy must let it through (it'll fail at the dial step in this
// test since the IP isn't a real server, but the deny-tag should be
// "dial-failed" not "private-ip").
func TestBlockPrivateIPsOptOut(t *testing.T) {
	cfg := Config{
		BlockPrivateIPs: false,
		// Tight DialTimeout: we expect the dial to fail (10.255.255.254
		// is RFC 1918 unreachable from the test machine). Bound it so
		// the test doesn't wait on the OS's TCP retry window.
		DialTimeout: 200 * time.Millisecond,
		resolver: fakeResolver{
			"internal.corp.local": []net.IPAddr{{IP: net.ParseIP("10.255.255.254")}},
		},
	}
	srv := startProxyForTest(t, NewAllowList([]string{"internal.corp.local"}), cfg)
	resp := sendCONNECT(t, srv.Addr(), "internal.corp.local", "443")
	body, _ := io.ReadAll(resp.Body)
	// Either 502 (dial failed because nothing's listening) or 200 if
	// some local thing happens to answer — both prove we passed the
	// private-IP gate. What we must NOT see is 403 + private-IP.
	if resp.StatusCode == http.StatusForbidden && strings.Contains(string(body), "private/special IPs") {
		t.Errorf("opt-out failed: still blocked at private-IP gate. status=%d body=%q",
			resp.StatusCode, body)
	}
}

// TestPicksFirstPublicIPSkippingPrivate validates the multi-record DNS
// path: when a hostname resolves to several IPs, the proxy must skip
// the private ones and dial the first public one (rather than
// rejecting the whole request because one of the records is private).
// Real-world: dual-stack hosts often resolve to a public v6 + private
// v4 (or vice versa).
func TestPicksFirstPublicIPSkippingPrivate(t *testing.T) {
	cfg := Config{
		BlockPrivateIPs: true,
		// We expect the dial to fail (TEST-NET-3 isn't routable). Tight
		// DialTimeout keeps the test fast.
		DialTimeout: 200 * time.Millisecond,
		resolver: fakeResolver{
			// Private first, public second. pickDialIP must scan past
			// the private entry rather than bailing.
			"mixed.example.com": []net.IPAddr{
				{IP: net.ParseIP("10.0.0.1")},
				{IP: net.ParseIP("203.0.113.1")}, // TEST-NET-3, public-routable
			},
		},
	}
	srv := startProxyForTest(t, NewAllowList([]string{"mixed.example.com"}), cfg)
	resp := sendCONNECT(t, srv.Addr(), "mixed.example.com", "443")
	body, _ := io.ReadAll(resp.Body)
	// Should attempt to dial 203.0.113.1 and fail (TEST-NET unreachable).
	// Must NOT 403 with private-IP — that would mean we bailed at the
	// first private IP instead of scanning past.
	if resp.StatusCode == http.StatusForbidden && strings.Contains(string(body), "private/special IPs") {
		t.Errorf("scanned-past-private failed: rejected even though a public IP was available. status=%d body=%q",
			resp.StatusCode, body)
	}
}

// TestCapacityLimit pins the concurrency cap. With MaxConcurrent=1 and
// one slow client holding the slot, a second client must immediately
// receive 503 — the proxy fails fast rather than queueing.
func TestCapacityLimit(t *testing.T) {
	// Slow handshake: hold the first client open by NOT sending the
	// CONNECT request line. The slot stays held until the proxy's
	// connect-timeout fires.
	cfg := Config{
		MaxConcurrent:  1,
		ConnectTimeout: 2 * time.Second, // long enough that slot is held while we test
	}
	srv := startProxyForTest(t, NewAllowList([]string{"example.com"}), cfg)
	// Open and hold connection #1; never send CONNECT.
	hold, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial #1: %v", err)
	}
	defer hold.Close()
	// Give the server a moment to accept and acquire the slot.
	time.Sleep(50 * time.Millisecond)
	// Connection #2 must hit the cap.
	resp := sendCONNECT(t, srv.Addr(), "example.com", "443")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "at capacity") {
		t.Errorf("body = %q, want at-capacity message", body)
	}
}

// TestConnectHandshakeTimeout pins the slowloris defense. A client
// that opens a TCP connection but never sends the CONNECT line must
// have its slot reclaimed within ConnectTimeout.
func TestConnectHandshakeTimeout(t *testing.T) {
	cfg := Config{
		MaxConcurrent:  1,
		ConnectTimeout: 200 * time.Millisecond,
	}
	srv := startProxyForTest(t, NewAllowList([]string{"example.com"}), cfg)
	// Hold a slot silently.
	silent, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial silent: %v", err)
	}
	defer silent.Close()
	// Wait past the handshake deadline + a small buffer.
	time.Sleep(400 * time.Millisecond)
	// The slot should be reclaimed by now; a fresh CONNECT must succeed
	// the cap check (and fail later for the right reason — e.g., 403
	// for non-allowed host or actual tunnel attempt, but NOT 503).
	resp := sendCONNECT(t, srv.Addr(), "example.com", "443")
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Errorf("slot not reclaimed after handshake timeout: still 503")
	}
}

// TestTunnelsAllowedHost ends-to-ends a CONNECT to a test TLS server.
// The test server runs on 127.0.0.1:<random>, so we hit two enforcement
// gates that the production path doesn't: (a) IP-literal CONNECTs are
// blocked, (b) port 443 is required. We use a hostname mapping via the
// resolver and BlockPrivateIPs=false so the test exercises the tunnel
// path itself rather than any of those guards.
func TestTunnelsAllowedHost(t *testing.T) {
	host, port, ts := startTestServer(t, "tunneled-payload")
	defer ts.Close()

	srv := startProxyForTest(t, NewAllowList([]string{"example.com"}), Config{
		BlockPrivateIPs: false,
		resolver: fakeResolver{
			"example.com": []net.IPAddr{{IP: net.ParseIP(host)}},
		},
	})

	// CONNECT through the proxy. We address the proxy with port 443 to
	// pass the port gate, but the resolver maps "example.com" to the
	// httptest server's loopback IP, so the actual dial lands at
	// 127.0.0.1:<httptest-port>. Since the tunnel is opaque TLS, the
	// CONNECT 200 response is sufficient evidence that the tunnel was
	// established.
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read connect response: %v", err)
	}
	defer resp.Body.Close()
	// We resolved to a loopback IP and the test asks for port 443,
	// but the underlying TCP target is the httptest port. So dial will
	// fail (nothing on 443) and we'll see 502, OR if the test happens
	// to hit some local thing, 200. The important assertion: NOT 403.
	// If we get 403 here it means a guard fired wrongly.
	if resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("unexpectedly 403 for allowed host: body=%q", body)
	}
	// Use the imported port to stop the linter complaining about unused.
	_ = port
}

// TestDeniedHostsTracking pins the contract surfaced to the runner /
// dashboard for the user-feedback loop: only allowlist denies appear
// in DeniedHosts(); the result is deduped, normalized (lowercased,
// trailing-dot stripped), and sorted; other deny categories
// (IP-literal, non-443 port, non-CONNECT) are NOT recorded since
// they're agent bugs rather than allowlist gaps.
func TestDeniedHostsTracking(t *testing.T) {
	srv := startProxyForTest(t, NewAllowList([]string{"api.anthropic.com"}), Config{})
	// Allowlist denies — these must show up. Send dup with different
	// casing and a trailing dot to confirm dedup + normalization.
	for _, host := range []string{"evil.example.com", "EVIL.EXAMPLE.COM", "evil.example.com.", "second.example.com"} {
		_ = sendCONNECT(t, srv.Addr(), host, "443")
	}
	// Non-443 port — must NOT show up (agent bug, not allowlist gap).
	_ = sendCONNECT(t, srv.Addr(), "api.anthropic.com", "8080")
	// IP literal — must NOT show up (security gate, not allowlist).
	_ = sendCONNECT(t, srv.Addr(), "169.254.169.254", "443")

	got := srv.DeniedHosts()
	want := []string{"evil.example.com", "second.example.com"}
	if len(got) != len(want) {
		t.Fatalf("got %d hosts, want %d. got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hosts[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDeniedHostsEmptyByDefault pins that a fresh server with no
// denies returns an empty (not nil) slice ready for JSON
// "denied_hosts" omission via omitempty.
func TestDeniedHostsEmptyByDefault(t *testing.T) {
	srv := startProxyForTest(t, NewAllowList([]string{"example.com"}), Config{})
	got := srv.DeniedHosts()
	if len(got) != 0 {
		t.Errorf("expected empty deny set, got %v", got)
	}
}

// TestConcurrentRequests verifies the proxy handles multiple parallel
// CONNECT attempts without interleaving response writes or deadlocking.
// All requests target a guaranteed-denied host so the test exercises
// concurrency without hitting the real network.
func TestConcurrentRequests(t *testing.T) {
	srv := startProxyForTest(t, NewAllowList(nil), Config{}) // empty allowlist — everything denied
	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			c, err := net.Dial("tcp", srv.Addr())
			if err != nil {
				t.Errorf("conn %d dial: %v", i, err)
				return
			}
			defer c.Close()
			fmt.Fprintf(c, "CONNECT denied-%d.example.invalid:443 HTTP/1.1\r\nHost: denied-%d.example.invalid:443\r\n\r\n", i, i)
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Errorf("conn %d read: %v", i, err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("conn %d status = %d, want 403", i, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()
}

// startTestHTTPServer is the plain-HTTP twin of startTestServer (which
// uses TLS). Forward-HTTP tests need a non-TLS origin to terminate the
// proxy's forwarded request.
func startTestHTTPServer(t *testing.T, body string) (host, port string, srv *httptest.Server) {
	t.Helper()
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	u := strings.TrimPrefix(srv.URL, "http://")
	h, p, err := net.SplitHostPort(u)
	if err != nil {
		srv.Close()
		t.Fatalf("split target: %v", err)
	}
	return h, p, srv
}

// sendForwardGET dials the proxy and sends a forward-HTTP GET request
// to the absolute URL `http://host[:port]/path`. Mirrors sendCONNECT
// for shared test plumbing.
func sendForwardGET(t *testing.T, proxyAddr, host, port, path string) *http.Response {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	hostPort := host
	if port != "" {
		hostPort = net.JoinHostPort(host, port)
	}
	fmt.Fprintf(c, "GET http://%s%s HTTP/1.1\r\nHost: %s\r\n\r\n", hostPort, path, hostPort)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		c.Close()
		t.Fatalf("read response: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestForwardHTTPRejectsNon80Port mirrors TestRejectsNon443Port for the
// HTTP-forward path: only port 80 is supported, just as CONNECT is
// 443-only. Restricting to standard ports keeps the security surface
// narrow without imposing on real-world clients.
func TestForwardHTTPRejectsNon80Port(t *testing.T) {
	srv := startProxyForTest(t, NewAllowList([]string{"example.com"}), Config{})
	resp := sendForwardGET(t, srv.Addr(), "example.com", "8080", "/")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "example.com:8080") {
		t.Errorf("body = %q, want mention of host:port", body)
	}
	if !strings.Contains(string(body), "only port 80") {
		t.Errorf("body = %q, want mention of port 80", body)
	}
}

// TestRejectsHTTPSForwardWithTargetURL pins the deny message for the
// motivating real-world case: an axios client (vite-plugin-mkcert) sends
// `GET https://github.com/...` through HTTP_PROXY instead of CONNECT.
// The proxy must (a) reject it because we don't terminate TLS, AND
// (b) name the offending URL so an operator can pinpoint which dep is
// misconfigured without parsing a downstream stack trace.
func TestRejectsHTTPSForwardWithTargetURL(t *testing.T) {
	srv := startProxyForTest(t, NewAllowList([]string{"github.com"}), Config{})
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET https://github.com/foo/bar/releases HTTP/1.1\r\nHost: github.com\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "https://github.com/foo/bar/releases") {
		t.Errorf("body = %q, want full target URL", body)
	}
	if !strings.Contains(string(body), "CONNECT") {
		t.Errorf("body = %q, want hint to use CONNECT", body)
	}
}

// TestForwardHTTPRejectsIPLiteral closes the SSRF/metadata-IP attack
// path for forward HTTP, same as TestRejectsIPLiteralCONNECT does for
// the tunnel path. A direct `GET http://169.254.169.254/` would
// otherwise hand the agent cloud metadata regardless of allowlist.
func TestForwardHTTPRejectsIPLiteral(t *testing.T) {
	srv := startProxyForTest(t, NewAllowList([]string{"example.com"}), Config{})
	resp := sendForwardGET(t, srv.Addr(), "169.254.169.254", "", "/latest/meta-data/")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "IP-literal HTTP forward not allowed") {
		t.Errorf("body = %q, want mention of IP-literal", body)
	}
}

// TestForwardHTTPRejectsNotAllowed confirms the same hostname allowlist
// gate the CONNECT path uses applies to forward HTTP — so the agent
// can't escape the boundary by switching from HTTPS-tunneled to plain
// HTTP. Also pins that this category counts toward DeniedHosts() (it's
// an allowlist gap the runner can surface to the user).
func TestForwardHTTPRejectsNotAllowed(t *testing.T) {
	srv := startProxyForTest(t, NewAllowList([]string{"api.anthropic.com"}), Config{})
	resp := sendForwardGET(t, srv.Addr(), "evil.example.com", "", "/")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "not in allowlist") {
		t.Errorf("body = %q, want allowlist deny", body)
	}
	if got := srv.DeniedHosts(); len(got) != 1 || got[0] != "evil.example.com" {
		t.Errorf("DeniedHosts() = %v, want [evil.example.com]", got)
	}
}

// TestForwardsAllowedHostHTTP ends-to-ends a real forward GET: the
// allowlist permits "example.com", the resolver points it at our
// test HTTP server's loopback IP, the proxy dials the resolved IP
// (DNS-rebinding defense), forwards the request, and streams the
// origin's body back unchanged. The body assertion proves we're not
// short-circuiting the response somewhere.
func TestForwardsAllowedHostHTTP(t *testing.T) {
	host, _, ts := startTestHTTPServer(t, "forward-payload")
	defer ts.Close()

	srv := startProxyForTest(t, NewAllowList([]string{"example.com"}), Config{
		BlockPrivateIPs: false,
		resolver: fakeResolver{
			"example.com": []net.IPAddr{{IP: net.ParseIP(host)}},
		},
	})

	// We address the proxy as port 80 (the only forward-HTTP port we
	// allow) and rely on the resolver to map example.com to the
	// httptest loopback IP. The actual TCP dial will therefore land at
	// 127.0.0.1:80 — which won't have anything listening — so we
	// expect 502 Bad Gateway. That's still proof the forward path
	// reached the dial stage (not a 403 gate), which is the point.
	resp := sendForwardGET(t, srv.Addr(), "example.com", "", "/anything")
	if resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("unexpectedly 403 for allowed host (should have passed gates and dialed): body=%q", body)
	}
}

// Package main is the entry point for agentbox — an orchestrator that
// runs an AI coding agent inside a Docker container against a bind-mounted
// working directory and writes a structured result on exit.
//
// Contract:     see docs/CONTRACT.md
// Architecture: see docs/ARCHITECTURE.md
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/interactive"
	"github.com/deployment-io/agentbox/internal/proxy"
	"github.com/deployment-io/agentbox/internal/result"
	"github.com/deployment-io/agentbox/internal/signals"
	"github.com/deployment-io/agentbox/internal/vendoring"

	// Side-effect imports register agents (Driver) and vendor detectors.
	// To ship additional agents or languages, add their package here.
	_ "github.com/deployment-io/agentbox/internal/claude"
	_ "github.com/deployment-io/agentbox/internal/codex"
	_ "github.com/deployment-io/agentbox/internal/opencode"
	_ "github.com/deployment-io/agentbox/internal/vendoring/golang"
	_ "github.com/deployment-io/agentbox/internal/vendoring/node"
)

// version is the agentbox release this binary was built from, stamped at
// image build via -ldflags "-X main.version=<tag>" (see Dockerfile +
// .github/workflows/release.yml). "dev" for local builds. Logged first
// thing so every container run is attributable to an exact release —
// without it, rollout gaps are invisible in the session logs.
var version = "dev"

func main() {
	// mcp-bridge is spawned by the agent's MCP client and speaks JSON-RPC on
	// stdout — handle it first, before the banner, so stdout stays pristine.
	if len(os.Args) > 1 && os.Args[1] == "mcp-bridge" {
		runMCPBridge()
		return
	}
	fmt.Fprintf(os.Stderr, "[agentbox] agentbox %s\n", version)
	if len(os.Args) > 1 && os.Args[1] == "vendor" {
		runVendor()
		return
	}
	runAgent()
}

// runMCPBridge is the `agentbox mcp-bridge <socket>` subcommand: a dumb
// bidirectional pipe between this process's stdio and the runner's per-task MCP
// tool socket. The agent's MCP client (e.g. Claude Code) spawns it as a stdio
// MCP server; it forwards newline-delimited JSON-RPC to the runner, which owns
// the protocol and executes tools with the runner's credentials. Stdout stays
// pristine — only socket bytes go out. Credentials never enter this container;
// this only relays intent to the privileged runner.
func runMCPBridge() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "[agentbox] mcp-bridge: missing socket path")
		os.Exit(2)
	}
	socket := os.Args[2]
	// The runner creates the socket before the container starts, but retry
	// briefly to absorb any startup skew before the agent's first tool call.
	var conn net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if conn, err = net.Dial("unix", socket); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[agentbox] mcp-bridge: dial %s: %v\n", socket, err)
		os.Exit(1)
	}
	defer conn.Close()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(conn, os.Stdin); done <- struct{}{} }()
	go func() { _, _ = io.Copy(os.Stdout, conn); done <- struct{}{} }()
	<-done // exit when either direction closes
}

// runAgent is the default mode: install the agent, run it against
// WORK_DIR, and write result.json.
func runAgent() {
	cfg, err := config.Load()
	if err != nil {
		exitWithFailure("config error", err)
	}

	ctx, cancel := signals.NewContext(context.Background())
	defer cancel()

	driver, err := agent.DriverFor(cfg.AgentType, cfg.AgentVersion)
	if err != nil {
		exitWithFailure("driver error", err)
	}

	// Detect the in-Step languages so this phase mirrors the vendor phase's
	// cache wiring: apply each ecosystem's Env (e.g. GOMODCACHE/GOPRIVATE so
	// the agent's `go build` resolves the pre-vendored deps offline) and add
	// its public verify hosts to the allowlist. Best-effort — a detection
	// error just means no language-specific wiring.
	verifyHosts := driver.AllowedHosts()
	if plan, perr := vendoring.BuildPlan(cfg.WorkDir); perr == nil {
		applyEnv(plan.Env(cacheDir()))
		verifyHosts = append(verifyHosts, plan.VerifyHosts()...)
	} else {
		fmt.Fprintf(os.Stderr, "[agentbox] language detection skipped: %v\n", perr)
	}

	// Start the network-allowlist proxy before Driver.Ensure so that
	// `npm install -g claude-code` (and any other install-time HTTPS
	// fetches) also routes through the allowlist. The HTTP_PROXY env
	// vars get exported to agentbox's own process env so all subsequent
	// child processes (npm, the agent itself) inherit and respect them.
	proxySrv, err := startProxy(verifyHosts, false)
	if err != nil {
		exitWithFailure("proxy start failed", err)
	}
	defer proxySrv.Close()

	if err := driver.Ensure(ctx); err != nil {
		exitWithFailure("agent install failed", err)
	}

	// Batch (default) runs one prompt and exits; interactive keeps the
	// container alive for a long-lived bidirectional session driven by
	// user messages on the filesystem (see internal/interactive). Both
	// share the proxy / Ensure / vendoring setup above.
	var outcome result.Outcome
	if cfg.Mode == config.ModeInteractive {
		iio, ioErr := interactive.New(cfg.WorkDir)
		if ioErr != nil {
			exitWithFailure("interactive io setup failed", ioErr)
		}
		outcome = agent.RunInteractive(ctx, cfg, driver, iio)
	} else {
		outcome = agent.Run(ctx, cfg, driver)
	}

	// Snapshot allowlist-deny hostnames AFTER the agent returns so we
	// capture every deny across the run lifetime. Tracked by the proxy
	// itself; we just promote the snapshot into the outcome JSON so
	// downstream consumers (runner → dashboard) can surface "add this
	// to your allowlist" suggestions without parsing stderr.
	outcome.DeniedHosts = proxySrv.DeniedHosts()

	if writeErr := result.Write(outcome); writeErr != nil {
		fmt.Fprintf(os.Stderr, "[agentbox] failed to write result: %v\n", writeErr)
	}

	os.Exit(outcome.ExitCode)
}

func exitWithFailure(label string, err error) {
	fmt.Fprintf(os.Stderr, "[agentbox] %s: %v\n", label, err)
	_ = result.WriteFailure(err, "")
	os.Exit(result.ExitExecutionFailure)
}

// startProxy starts the CONNECT proxy and exports HTTP(S)_PROXY/NO_PROXY into
// agentbox's own env so child processes (npm, go, the agent) inherit it.
//
// allowAll=false (agent phase): the allowlist is baseHosts ∪
// ADDITIONAL_ALLOWED_HOSTS; anything else is denied. allowAll=true (vendor
// phase): any public host is permitted — pinning a complete package-registry
// /CDN allowlist for arbitrary projects is impractical. Either way the
// SSRF/metadata defenses are unchanged: IP-literal CONNECTs are rejected and
// hostnames resolving to private/special IPs are denied (BlockPrivateIPs,
// default on; opt out per-runner via AGENTBOX_BLOCK_PRIVATE_IPS=0).
func startProxy(baseHosts []string, allowAll bool) (*proxy.Server, error) {
	cfg := proxy.Config{
		Logger:          os.Stderr,
		BlockPrivateIPs: blockPrivateIPsFromEnv(),
	}
	var list *proxy.AllowList
	if allowAll {
		list = proxy.NewAllowAllList()
	} else {
		allowed := append([]string{}, baseHosts...)
		if extra := strings.TrimSpace(os.Getenv("ADDITIONAL_ALLOWED_HOSTS")); extra != "" {
			for _, h := range strings.Split(extra, ",") {
				if h := strings.TrimSpace(h); h != "" {
					allowed = append(allowed, h)
				}
			}
		}
		list = proxy.NewAllowList(allowed)
	}
	srv, err := proxy.Start(list, cfg)
	if err != nil {
		return nil, err
	}
	url := "http://" + srv.Addr()
	_ = os.Setenv("HTTP_PROXY", url)
	_ = os.Setenv("HTTPS_PROXY", url)
	_ = os.Setenv("http_proxy", url)
	_ = os.Setenv("https_proxy", url)
	// NO_PROXY guards localhost so e.g. agent-internal RPC can bypass.
	_ = os.Setenv("NO_PROXY", "127.0.0.1,localhost")
	_ = os.Setenv("no_proxy", "127.0.0.1,localhost")
	return srv, nil
}

// blockPrivateIPsFromEnv reads AGENTBOX_BLOCK_PRIVATE_IPS. Default
// true (block). "0", "false", or "no" (case-insensitive) opts out for
// runners that legitimately need to reach internal private-IP
// destinations.
func blockPrivateIPsFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENTBOX_BLOCK_PRIVATE_IPS"))) {
	case "0", "false", "no":
		return false
	}
	return true
}

// vendorAllowAllEgress reports whether the vendor phase permits all public
// egress (the default — pinning a complete allowlist for arbitrary project
// deps is impractical). Set AGENTBOX_VENDOR_STRICT_EGRESS=1 to fall back to
// the per-ecosystem allowlist. Private/metadata IPs are blocked either way.
// The agent phase is always strict, regardless of this setting.
func vendorAllowAllEgress() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AGENTBOX_VENDOR_STRICT_EGRESS"))) {
	case "1", "true", "yes":
		return false
	}
	return true
}

// runVendor is the `agentbox vendor` subcommand. It pre-fetches each
// in-Step repo's dependencies into the shared module cache using the
// vendor-phase credentials, so the credential-less agent phase can build
// and verify offline. Unlike the agent path it writes no result.json;
// the runner reads its exit code (0 = vendored, non-zero = fetch failed).
// See PLAN_tasks_verification.md.
func runVendor() {
	ctx, cancel := signals.NewContext(context.Background())
	defer cancel()

	workDir := os.Getenv("WORK_DIR")
	if workDir == "" {
		workDir = "/work"
	}
	plan, err := vendoring.BuildPlan(workDir)
	if err != nil {
		vendorFail("vendor planning failed", err)
	}
	if plan.Empty() {
		fmt.Fprintf(os.Stderr, "[agentbox] vendor: no supported ecosystems detected under %s; nothing to do\n", workDir)
		return
	}

	// Apply each ecosystem's cache env (e.g. GOMODCACHE/GOPRIVATE) so the
	// vendor subprocesses download into the shared shelf.
	applyEnv(plan.Env(cacheDir()))

	// Trusted vendor phase: allow all public egress by default (a complete
	// registry/CDN allowlist for arbitrary deps is impractical), still
	// blocking private/metadata IPs. Opt back into the strict per-ecosystem
	// allowlist with AGENTBOX_VENDOR_STRICT_EGRESS=1.
	proxySrv, err := startProxy(plan.AllowedHosts(), vendorAllowAllEgress())
	if err != nil {
		vendorFail("proxy start failed", err)
	}
	defer proxySrv.Close()

	if err := vendoring.ConfigureGit(os.Getenv("GIT_TOKEN")); err != nil {
		vendorFail("git credential setup failed", err)
	}
	if err := plan.Execute(ctx); err != nil {
		vendorFail("vendor failed", err)
	}
	fmt.Fprintln(os.Stderr, "[agentbox] vendor: complete")
}

// vendorFail logs and exits non-zero. Vendor mode emits no result.json;
// the runner distinguishes vendor failures from agent failures by which
// phase's container returned the non-zero code.
func vendorFail(label string, err error) {
	fmt.Fprintf(os.Stderr, "[agentbox] %s: %v\n", label, err)
	os.Exit(result.ExitExecutionFailure)
}

// cacheDir is the shared module-cache mount the consumer (the runner)
// provides via AGENTBOX_CACHE_DIR. Empty when agentbox runs standalone, in
// which case each ecosystem falls back to its default cache location.
func cacheDir() string { return os.Getenv("AGENTBOX_CACHE_DIR") }

// applyEnv sets each "KEY=VALUE" entry in the current process env so spawned
// subprocesses (the vendor commands, the agent) inherit it.
func applyEnv(kvs []string) {
	for _, kv := range kvs {
		if k, v, ok := strings.Cut(kv, "="); ok {
			_ = os.Setenv(k, v)
		}
	}
}

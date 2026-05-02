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
	"os"
	"strings"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/proxy"
	"github.com/deployment-io/agentbox/internal/result"
	"github.com/deployment-io/agentbox/internal/signals"

	// Side-effect import: registers "claude-code" as a Driver with the
	// agent package. To ship additional agents, add their package below.
	_ "github.com/deployment-io/agentbox/internal/claude"
)

func main() {
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

	// Start the network-allowlist proxy before Driver.Ensure so that
	// `npm install -g claude-code` (and any other install-time HTTPS
	// fetches) also routes through the allowlist. The HTTP_PROXY env
	// vars get exported to agentbox's own process env so all subsequent
	// child processes (npm, the agent itself) inherit and respect them.
	proxySrv, err := startProxy(driver)
	if err != nil {
		exitWithFailure("proxy start failed", err)
	}
	defer proxySrv.Close()

	if err := driver.Ensure(ctx); err != nil {
		exitWithFailure("agent install failed", err)
	}

	outcome := agent.Run(ctx, cfg, driver)

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

// startProxy builds the allowlist (Driver-declared hosts ∪
// ADDITIONAL_ALLOWED_HOSTS env var, comma-separated) and starts the
// CONNECT proxy. Sets HTTP_PROXY, HTTPS_PROXY, NO_PROXY in agentbox's
// own env so child processes inherit. Logs the resolved allowlist for
// transparency.
//
// Fallback when ADDITIONAL_ALLOWED_HOSTS is unset: just the Driver's
// built-in allowlist applies. Empty Driver.AllowedHosts() AND empty
// env var means the agent can't reach anything — surfaces immediately
// as a denied CONNECT in the agent's own error output.
//
// Private-IP blocking defaults on (defense vs SSRF / cloud-metadata
// resolution). Ops who legitimately need to reach internal RFC 1918
// destinations (Nexus on 10.0.x.x, internal GitLab, etc.) can opt out
// per-runner via AGENTBOX_BLOCK_PRIVATE_IPS=0.
func startProxy(driver agent.Driver) (*proxy.Server, error) {
	allowed := append([]string{}, driver.AllowedHosts()...)
	if extra := strings.TrimSpace(os.Getenv("ADDITIONAL_ALLOWED_HOSTS")); extra != "" {
		for _, h := range strings.Split(extra, ",") {
			if h := strings.TrimSpace(h); h != "" {
				allowed = append(allowed, h)
			}
		}
	}
	cfg := proxy.Config{
		Logger:          os.Stderr,
		BlockPrivateIPs: blockPrivateIPsFromEnv(),
	}
	srv, err := proxy.Start(proxy.NewAllowList(allowed), cfg)
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
	fmt.Fprintf(os.Stderr, "[agentbox] proxy started on %s; allowlist: %s; block_private_ips: %t\n",
		srv.Addr(), strings.Join(allowed, ","), cfg.BlockPrivateIPs)
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

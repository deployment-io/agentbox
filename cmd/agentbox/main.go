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

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/config"
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

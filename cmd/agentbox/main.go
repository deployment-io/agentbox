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
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		exitWithFailure("config error", err)
	}

	ctx, cancel := signals.NewContext(context.Background())
	defer cancel()

	installer, err := agent.InstallerFor(cfg.AgentType, cfg.AgentVersion)
	if err != nil {
		exitWithFailure("installer error", err)
	}

	if err := installer.Ensure(ctx); err != nil {
		exitWithFailure("agent install failed", err)
	}

	outcome := agent.Run(ctx, cfg, installer)

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

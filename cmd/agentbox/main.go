// Package main is the entry point for agentbox — an orchestrator that
// runs an AI coding agent (Claude Code in v1) inside a Docker container
// against a bind-mounted working directory and writes a structured result
// on exit.
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
		fmt.Fprintf(os.Stderr, "[agentbox] config error: %v\n", err)
		_ = result.WriteFailure(err, "")
		os.Exit(result.ExitExecutionFailure)
	}

	ctx, cancel := signals.NewContext(context.Background())
	defer cancel()

	outcome := agent.Run(ctx, cfg)

	if writeErr := result.Write(outcome); writeErr != nil {
		fmt.Fprintf(os.Stderr, "[agentbox] failed to write result: %v\n", writeErr)
	}

	os.Exit(outcome.ExitCode)
}

// Package agent runs the Claude Code subprocess and captures its outcome.
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
)

// shutdownGrace is how long we wait for the Claude Code subprocess to
// exit cleanly after forwarding SIGTERM, before escalating to SIGKILL.
// Matches docs/CONTRACT.md's signal handling spec.
const shutdownGrace = 10 * time.Second

// Run spawns Claude Code as a subprocess, streams its stdout/stderr to
// the container's stdout/stderr, and returns an Outcome describing what
// happened. The returned Outcome is always valid — callers can write it
// to /result.json regardless of success or failure.
//
// On ctx cancellation (SIGTERM/SIGINT received by agentbox), the handler
// forwards SIGTERM to the subprocess, waits up to shutdownGrace for a
// clean exit, then escalates to SIGKILL.
//
// v0 scope: happy-path success + cancelled + generic execution failure.
// TODO(phase-A): parse Claude Code's stream-json for changes_summary,
// files_changed, token_usage, turns; distinguish auth failures (exit 2);
// implement no-activity detector.
func Run(ctx context.Context, cfg *config.Config) result.Outcome {
	cmd := exec.Command("claude", buildArgs(cfg)...)
	cmd.Dir = cfg.WorkDir
	cmd.Env = buildEnv(cfg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return result.Outcome{
			Status:   result.StatusFailure,
			ExitCode: result.ExitExecutionFailure,
			Error:    fmt.Sprintf("failed to start claude: %v", err),
		}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return classifyOutcome(err)
	case <-ctx.Done():
		return gracefulShutdown(cmd, done)
	}
}

// buildArgs constructs the command-line arguments for the Claude Code
// invocation. Pure function of cfg — unit-testable without spawning.
func buildArgs(cfg *config.Config) []string {
	args := []string{
		"-p", cfg.StepPrompt,
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
	}
	if cfg.MaxTurns != "" {
		args = append(args, "--max-turns", cfg.MaxTurns)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	return args
}

// buildEnv constructs the environment for the Claude Code subprocess.
// Both credential paths (Anthropic Direct and Bedrock) work via env-var
// pass-through: Claude Code itself dispatches based on
// CLAUDE_CODE_USE_BEDROCK. agentbox's role is validation + pass-through.
func buildEnv(cfg *config.Config) []string {
	env := os.Environ()
	if cfg.PreviousStepsSummary != "" {
		env = append(env, "PREVIOUS_STEPS_SUMMARY="+cfg.PreviousStepsSummary)
	}
	return env
}

// gracefulShutdown forwards SIGTERM to the subprocess, waits up to
// shutdownGrace for it to exit, then escalates to SIGKILL. Returns a
// cancelled Outcome regardless of how the subprocess exited.
func gracefulShutdown(cmd *exec.Cmd, done <-chan error) result.Outcome {
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	select {
	case <-done:
		// Subprocess exited within the grace period — clean shutdown.
	case <-time.After(shutdownGrace):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done // reap the zombie
	}

	return result.Outcome{
		Status:   result.StatusCancelled,
		ExitCode: result.ExitCancelled,
		Error:    "cancelled by signal",
	}
}

// classifyOutcome maps a subprocess exit error to a result.Outcome.
// Called on the natural-exit path (no cancellation).
func classifyOutcome(err error) result.Outcome {
	if err == nil {
		return result.Outcome{
			Status:   result.StatusSuccess,
			ExitCode: result.ExitSuccess,
		}
	}

	// TODO(phase-A): parse stream-json / exit code for auth-failure
	// detection (returning ExitAuthFailure). For now, any non-zero exit
	// is an execution failure.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return result.Outcome{
			Status:   result.StatusFailure,
			ExitCode: result.ExitExecutionFailure,
			Error:    fmt.Sprintf("claude exited with error: %v", err),
		}
	}

	return result.Outcome{
		Status:   result.StatusFailure,
		ExitCode: result.ExitExecutionFailure,
		Error:    fmt.Sprintf("failed to run claude: %v", err),
	}
}

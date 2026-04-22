// Package agent runs the Claude Code subprocess and captures its outcome.
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
)

// Run spawns Claude Code as a subprocess, streams its stdout/stderr to
// the container's stdout/stderr, and returns an Outcome describing what
// happened. The returned Outcome is always valid — callers can write it
// to /result.json regardless of success or failure.
//
// v0 scope: happy-path success + cancelled + generic execution failure.
// TODO(phase-A): parse Claude Code's stream-json for changes_summary,
// files_changed, token_usage, turns; distinguish auth failures (exit 2);
// implement no-activity detector.
func Run(ctx context.Context, cfg *config.Config) result.Outcome {
	args := buildArgs(cfg)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = cfg.WorkDir
	cmd.Env = buildEnv(cfg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return classifyError(ctx, err)
	}

	return result.Outcome{
		Status:   result.StatusSuccess,
		ExitCode: result.ExitSuccess,
	}
}

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
// Both credential paths (Anthropic Direct and Bedrock) are handled by
// pass-through: Claude Code itself dispatches based on the presence of
// CLAUDE_CODE_USE_BEDROCK. agentbox's role is validation + pass-through.
func buildEnv(cfg *config.Config) []string {
	env := os.Environ()

	if cfg.PreviousStepsSummary != "" {
		env = append(env, "PREVIOUS_STEPS_SUMMARY="+cfg.PreviousStepsSummary)
	}

	return env
}

func classifyError(ctx context.Context, err error) result.Outcome {
	if errors.Is(ctx.Err(), context.Canceled) {
		return result.Outcome{
			Status:   result.StatusCancelled,
			ExitCode: result.ExitCancelled,
			Error:    "cancelled by signal",
		}
	}

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

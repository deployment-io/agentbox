// Package agent runs the Claude Code subprocess and captures its outcome.
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
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
// the container's stdout/stderr, parses its --output-format=stream-json
// events to populate the Outcome, and returns the result. The Outcome
// is always valid — callers can write it to /result.json regardless of
// success, failure, cancellation, or timeout.
//
// On ctx cancellation (SIGTERM/SIGINT received by agentbox), Run forwards
// SIGTERM to the subprocess, waits up to shutdownGrace for a clean exit,
// then escalates to SIGKILL. Any partial state accumulated by the parser
// (turns so far, files changed so far) is preserved in the cancelled
// Outcome.
func Run(ctx context.Context, cfg *config.Config) result.Outcome {
	agentVersion := DetectVersion()

	cmd := exec.Command("claude", buildArgs(cfg)...)
	cmd.Dir = cfg.WorkDir
	cmd.Env = buildEnv(cfg)

	parser := newStreamParser()
	pr, pw := io.Pipe()
	cmd.Stdout = io.MultiWriter(os.Stdout, pw)

	stderrBuf := &bytes.Buffer{}
	cmd.Stderr = io.MultiWriter(os.Stderr, stderrBuf)

	parseDone := make(chan struct{})
	go func() {
		defer close(parseDone)
		parser.Consume(pr)
	}()

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		<-parseDone
		return withVersion(agentVersion, result.Outcome{
			Status:   result.StatusFailure,
			ExitCode: result.ExitExecutionFailure,
			Error:    fmt.Sprintf("failed to start claude: %v", err),
		})
	}

	// Wait for subprocess in a goroutine. On Wait return, close the
	// pipe so the parser finishes; only then signal `done`. By the
	// time callers read `done`, parser state is safe to observe.
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = pw.Close()
		<-parseDone
		done <- err
	}()

	select {
	case err := <-done:
		return withVersion(agentVersion, buildOutcome(err, parser, stderrBuf.String()))
	case <-ctx.Done():
		return withVersion(agentVersion, gracefulShutdown(cmd, done, parser, stderrBuf))
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
// cancelled Outcome populated with whatever parser state was captured
// before cancellation.
func gracefulShutdown(cmd *exec.Cmd, done <-chan error, parser *streamParser, stderrBuf *bytes.Buffer) result.Outcome {
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	select {
	case <-done:
		// Subprocess exited within the grace period.
	case <-time.After(shutdownGrace):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done // reap the zombie
	}

	return result.Outcome{
		Status:         result.StatusCancelled,
		ExitCode:       result.ExitCancelled,
		Error:          "cancelled by signal",
		ChangesSummary: parser.changesSummary,
		FilesChanged:   parser.filesChangedSorted(),
		TokenUsage:     parser.usage,
		Turns:          parser.turns,
	}
}

// buildOutcome maps a natural subprocess exit into a result.Outcome.
// Called on the non-cancellation path. Success is narrower than "exit
// code 0": Claude Code can exit 0 with is_error: true in stream-json
// (e.g., error_max_turns). Both cases route through classifyFailure.
func buildOutcome(err error, parser *streamParser, stderrText string) result.Outcome {
	if err == nil && !parser.isError {
		return result.Outcome{
			Status:         result.StatusSuccess,
			ExitCode:       result.ExitSuccess,
			ChangesSummary: parser.changesSummary,
			FilesChanged:   parser.filesChangedSorted(),
			TokenUsage:     parser.usage,
			Turns:          parser.turns,
		}
	}
	return classifyFailure(err, parser, stderrText)
}

// classifyFailure constructs a failure Outcome with the right exit code.
// Auth / rate-limit failures get ExitAuthFailure (2) so consumers can
// surface an actionable message; everything else gets ExitExecutionFailure (1).
func classifyFailure(err error, parser *streamParser, stderrText string) result.Outcome {
	authFailure := parser.isAuthFailure() || hasAuthKeyword(strings.ToLower(stderrText))

	exitCode := result.ExitExecutionFailure
	if authFailure {
		exitCode = result.ExitAuthFailure
	}

	return result.Outcome{
		Status:         result.StatusFailure,
		ExitCode:       exitCode,
		Error:          failureMessage(err, parser),
		ChangesSummary: parser.changesSummary,
		FilesChanged:   parser.filesChangedSorted(),
		TokenUsage:     parser.usage,
		Turns:          parser.turns,
	}
}

// failureMessage produces a descriptive error string from either the
// subprocess exit error or the parser's recorded error state.
func failureMessage(err error, parser *streamParser) string {
	switch {
	case err != nil && isExitError(err):
		return fmt.Sprintf("claude exited with error: %v", err)
	case err != nil:
		return fmt.Sprintf("failed to run claude: %v", err)
	case parser.isError && parser.errorSubtype != "" && parser.changesSummary != "":
		return fmt.Sprintf("claude reported error (%s): %s", parser.errorSubtype, parser.changesSummary)
	case parser.isError && parser.errorSubtype != "":
		return fmt.Sprintf("claude reported error: %s", parser.errorSubtype)
	case parser.isError:
		return "claude reported error"
	default:
		return "claude reported error with no detail"
	}
}

// isExitError reports whether err is an *exec.ExitError (i.e., the
// subprocess ran but exited non-zero).
func isExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// withVersion returns o with AgentVersion populated. Keeps main.go
// and Run clean — the version is detected once per invocation and
// attached to every Outcome Run produces.
func withVersion(version string, o result.Outcome) result.Outcome {
	o.AgentVersion = version
	return o
}

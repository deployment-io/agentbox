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

// shutdownGrace is the SIGTERM → SIGKILL wait.
const shutdownGrace = 10 * time.Second

type cancelReason int

const (
	reasonSignal cancelReason = iota
	reasonTimeout
)

// Run spawns Claude Code, streams its output, parses stream-json, and
// returns an Outcome (success, failure, cancelled, or timeout). On
// cancellation or no-activity timeout, forwards SIGTERM with grace
// before SIGKILL.
func Run(ctx context.Context, cfg *config.Config) result.Outcome {
	agentVersion := DetectVersion()

	cmd := exec.Command("claude", buildArgs(cfg)...)
	cmd.Dir = cfg.WorkDir
	cmd.Env = buildEnv(cfg)

	parser := newStreamParser()
	pr, pw := io.Pipe()
	tracker := newActivityTracker()

	cmd.Stdout = io.MultiWriter(os.Stdout, pw, tracker)

	stderrBuf := &bytes.Buffer{}
	cmd.Stderr = io.MultiWriter(os.Stderr, stderrBuf, tracker)

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

	watcherCtx, stopWatcher := context.WithCancel(ctx)
	defer stopWatcher()
	timeoutReached := make(chan struct{}, 1)
	go watchActivity(watcherCtx, tracker, cfg.NoActivityTimeout, timeoutReached)

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
		return withVersion(agentVersion, gracefulShutdown(cmd, done, parser, reasonSignal, cfg.NoActivityTimeout))
	case <-timeoutReached:
		fmt.Fprintf(os.Stderr, "[agentbox] no agent output for %s; killing subprocess\n", cfg.NoActivityTimeout)
		return withVersion(agentVersion, gracefulShutdown(cmd, done, parser, reasonTimeout, cfg.NoActivityTimeout))
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

// buildEnv forwards the parent env plus optional extras. Credential
// path dispatch happens inside Claude Code via CLAUDE_CODE_USE_BEDROCK.
func buildEnv(cfg *config.Config) []string {
	env := os.Environ()
	if cfg.PreviousStepsSummary != "" {
		env = append(env, "PREVIOUS_STEPS_SUMMARY="+cfg.PreviousStepsSummary)
	}
	return env
}

func gracefulShutdown(cmd *exec.Cmd, done <-chan error, parser *streamParser, reason cancelReason, timeout time.Duration) result.Outcome {
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	select {
	case <-done:
	case <-time.After(shutdownGrace):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
	}

	base := result.Outcome{
		ChangesSummary: parser.changesSummary,
		FilesChanged:   parser.filesChangedSorted(),
		TokenUsage:     parser.usage,
		Turns:          parser.turns,
	}

	switch reason {
	case reasonTimeout:
		base.Status = result.StatusTimeout
		base.ExitCode = result.ExitTimeout
		base.Error = fmt.Sprintf("no agent output for %s; subprocess killed", timeout)
	default:
		base.Status = result.StatusCancelled
		base.ExitCode = result.ExitCancelled
		base.Error = "cancelled by signal"
	}
	return base
}

// buildOutcome maps a subprocess exit to an Outcome. Claude Code can
// exit 0 while reporting is_error in stream-json (e.g., max turns);
// both route through classifyFailure.
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

// classifyFailure picks exit code 2 for auth / rate-limit failures, 1 otherwise.
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

func isExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

func withVersion(version string, o result.Outcome) result.Outcome {
	o.AgentVersion = version
	return o
}

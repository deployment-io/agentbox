// Package agent runs the agent subprocess and captures its outcome.
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

// Run spawns the agent subprocess via the Installer, streams its output,
// parses stream-json, and returns an Outcome (success, failure, cancelled,
// or timeout). On cancellation or no-activity timeout, forwards SIGTERM
// with grace before SIGKILL.
func Run(ctx context.Context, cfg *config.Config, installer Installer) result.Outcome {
	agentVersion := installer.DetectVersion()

	cmd := exec.Command(installer.Binary(), installer.BuildArgs(cfg)...)
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
			Error:    fmt.Sprintf("failed to start %s: %v", installer.Binary(), err),
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
		return withVersion(agentVersion, buildOutcome(err, parser, stderrBuf.String(), installer.Binary()))
	case <-ctx.Done():
		return withVersion(agentVersion, gracefulShutdown(cmd, done, parser, reasonSignal, cfg.NoActivityTimeout))
	case <-timeoutReached:
		fmt.Fprintf(os.Stderr, "[agentbox] no agent output for %s; killing subprocess\n", cfg.NoActivityTimeout)
		return withVersion(agentVersion, gracefulShutdown(cmd, done, parser, reasonTimeout, cfg.NoActivityTimeout))
	}
}

// buildEnv forwards the parent env plus optional extras. Credential
// path dispatch happens inside the agent via CLAUDE_CODE_USE_BEDROCK.
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

// buildOutcome maps a subprocess exit to an Outcome. The agent can exit
// 0 while reporting is_error in stream-json (e.g., max turns); both
// route through classifyFailure.
func buildOutcome(err error, parser *streamParser, stderrText, binary string) result.Outcome {
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
	return classifyFailure(err, parser, stderrText, binary)
}

// classifyFailure picks exit code 2 for auth / rate-limit failures, 1 otherwise.
func classifyFailure(err error, parser *streamParser, stderrText, binary string) result.Outcome {
	authFailure := parser.isAuthFailure() || hasAuthKeyword(strings.ToLower(stderrText))

	exitCode := result.ExitExecutionFailure
	if authFailure {
		exitCode = result.ExitAuthFailure
	}

	return result.Outcome{
		Status:         result.StatusFailure,
		ExitCode:       exitCode,
		Error:          failureMessage(err, parser, binary),
		ChangesSummary: parser.changesSummary,
		FilesChanged:   parser.filesChangedSorted(),
		TokenUsage:     parser.usage,
		Turns:          parser.turns,
	}
}

func failureMessage(err error, parser *streamParser, binary string) string {
	switch {
	case err != nil && isExitError(err):
		return fmt.Sprintf("%s exited with error: %v", binary, err)
	case err != nil:
		return fmt.Sprintf("failed to run %s: %v", binary, err)
	case parser.isError && parser.errorSubtype != "" && parser.changesSummary != "":
		return fmt.Sprintf("%s reported error (%s): %s", binary, parser.errorSubtype, parser.changesSummary)
	case parser.isError && parser.errorSubtype != "":
		return fmt.Sprintf("%s reported error: %s", binary, parser.errorSubtype)
	case parser.isError:
		return binary + " reported error"
	default:
		return binary + " reported error with no detail"
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

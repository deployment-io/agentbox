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

// Run spawns the agent subprocess via the Driver, streams its output
// through the Driver's OutputParser, and returns an Outcome (success,
// failure, cancelled, or timeout). On cancellation or no-activity
// timeout, forwards SIGTERM with grace before SIGKILL.
func Run(ctx context.Context, cfg *config.Config, driver Driver) result.Outcome {
	agentVersion := driver.DetectVersion()

	cmd := exec.Command(driver.Binary(), driver.BuildArgs(cfg)...)
	cmd.Dir = cfg.WorkDir
	cmd.Env = buildEnv(cfg)

	parser := driver.NewOutputParser()
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
			Error:    fmt.Sprintf("failed to start %s: %v", driver.Binary(), err),
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
		return withVersion(agentVersion, buildOutcome(err, parser.State(), stderrBuf.String(), driver.Binary()))
	case <-ctx.Done():
		return withVersion(agentVersion, gracefulShutdown(cmd, done, parser, reasonSignal, cfg.NoActivityTimeout))
	case <-timeoutReached:
		fmt.Fprintf(os.Stderr, "[agentbox] no agent output for %s; killing subprocess\n", cfg.NoActivityTimeout)
		return withVersion(agentVersion, gracefulShutdown(cmd, done, parser, reasonTimeout, cfg.NoActivityTimeout))
	}
}

// buildEnv forwards the parent env plus optional extras. Credential-path
// dispatch is the agent's responsibility; agentbox just forwards.
func buildEnv(cfg *config.Config) []string {
	env := os.Environ()
	if cfg.PreviousStepsSummary != "" {
		env = append(env, "PREVIOUS_STEPS_SUMMARY="+cfg.PreviousStepsSummary)
	}
	return env
}

func gracefulShutdown(cmd *exec.Cmd, done <-chan error, parser OutputParser, reason cancelReason, timeout time.Duration) result.Outcome {
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

	state := parser.State()
	base := result.Outcome{
		ChangesSummary: state.ChangesSummary,
		FilesChanged:   state.FilesChanged,
		TokenUsage:     state.TokenUsage,
		Turns:          state.Turns,
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

// buildOutcome maps a subprocess exit to an Outcome. Some agents can
// exit 0 while reporting is_error in their output (e.g., max turns);
// both cases route through classifyFailure.
func buildOutcome(err error, state ParsedState, stderrText, binary string) result.Outcome {
	if err == nil && !state.IsError {
		return result.Outcome{
			Status:         result.StatusSuccess,
			ExitCode:       result.ExitSuccess,
			ChangesSummary: state.ChangesSummary,
			FilesChanged:   state.FilesChanged,
			TokenUsage:     state.TokenUsage,
			Turns:          state.Turns,
		}
	}
	return classifyFailure(err, state, stderrText, binary)
}

// classifyFailure picks exit code 2 for auth / rate-limit failures, 1 otherwise.
func classifyFailure(err error, state ParsedState, stderrText, binary string) result.Outcome {
	authFailure := state.IsAuthFailure || HasAuthKeyword(strings.ToLower(stderrText))

	exitCode := result.ExitExecutionFailure
	if authFailure {
		exitCode = result.ExitAuthFailure
	}

	return result.Outcome{
		Status:         result.StatusFailure,
		ExitCode:       exitCode,
		Error:          failureMessage(err, state, binary),
		ChangesSummary: state.ChangesSummary,
		FilesChanged:   state.FilesChanged,
		TokenUsage:     state.TokenUsage,
		Turns:          state.Turns,
	}
}

func failureMessage(err error, state ParsedState, binary string) string {
	switch {
	case err != nil && isExitError(err):
		return fmt.Sprintf("%s exited with error: %v", binary, err)
	case err != nil:
		return fmt.Sprintf("failed to run %s: %v", binary, err)
	case state.IsError && state.ChangesSummary != "":
		return fmt.Sprintf("%s reported error: %s", binary, state.ChangesSummary)
	case state.IsError:
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

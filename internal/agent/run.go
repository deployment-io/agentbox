package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/progress"
	"github.com/deployment-io/agentbox/internal/result"
)

// parserProgressSource adapts an OutputParser into a progress.Source.
// Returns the parser's current State as a flat Snapshot. Called by the
// progress writer's loop on its own cadence (independent of agent
// output rate).
type parserProgressSource struct{ p OutputParser }

func (s parserProgressSource) Snapshot() progress.Snapshot {
	st := s.p.State()
	return progress.Snapshot{
		Turns:           st.Turns,
		InputTokens:     st.TokenUsage.InputTokens,
		OutputTokens:    st.TokenUsage.OutputTokens,
		CacheReadTokens: st.TokenUsage.CacheReadTokens,
	}
}

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
//
// The returned Outcome carries metadata about what actually ran —
// AgentType (from cfg), AgentVersion (from Driver.DetectVersion),
// Model (last value the parser observed in the agent's output stream,
// when exposed), and StartedAt/EndedAt unix-second timestamps around
// the subprocess. These are populated via a single deferred snapshot
// regardless of which return path fires so the result file is
// self-describing for any exit type.
func Run(ctx context.Context, cfg *config.Config, driver Driver) (outcome result.Outcome) {
	agentVersion := driver.DetectVersion()

	cmd := exec.Command(driver.Binary(), driver.BuildArgs(cfg)...)
	cmd.Dir = cfg.WorkDir
	cmd.Env = buildEnv(cfg)

	parser := driver.NewOutputParser()
	pr, pw := io.Pipe()
	tracker := newActivityTracker()

	// Phase 5.5b: live progress snapshots into <result-dir>/progress.json.
	// The runner polls the file on its existing 5s heartbeat cadence and
	// forwards the snapshot to the server, where the dashboard surfaces
	// it as live turn / token counters. Stop is deferred so the final
	// flush happens before Run returns — without it, the heartbeat
	// following Run completion would see a snapshot up to WriteInterval
	// stale (or worse, the file might not exist yet for a fast run).
	progressWriter := progress.NewWriter(filepath.Dir(result.Path()), parserProgressSource{p: parser})
	progressWriter.Start()
	defer progressWriter.Stop()

	// Replace os.Stdout in the MultiWriter with a Driver-supplied
	// formatter that turns the agent's native output (e.g., Claude
	// Code's stream-json) into compact one-line summaries before the
	// bytes reach the container's stdout. The parser pipe (pw) still
	// sees the raw stream for structured capture; nothing about
	// result.json or progress.json changes.
	humanOut := driver.NewLogFormatter(os.Stdout)
	defer humanOut.Close()

	cmd.Stdout = io.MultiWriter(humanOut, pw, tracker)

	stderrBuf := &bytes.Buffer{}
	cmd.Stderr = io.MultiWriter(os.Stderr, stderrBuf, tracker)

	parseDone := make(chan struct{})
	go func() {
		defer close(parseDone)
		parser.Consume(pr)
	}()

	// startedAt is captured immediately before cmd.Start so the value
	// reflects when agentbox actually launched the subprocess; endedAt
	// is captured the moment cmd.Wait returns (or, on a Start failure,
	// right after the failed Start). All return paths through this
	// function ensure parseDone has closed before the deferred snapshot
	// reads parser.State(), so the model lookup races neither Consume
	// nor a torn read.
	var startedAt, endedAt int64
	defer func() {
		outcome.AgentType = cfg.AgentType
		outcome.AgentVersion = agentVersion
		outcome.StartedAt = startedAt
		outcome.EndedAt = endedAt
		outcome.Model = parser.State().Model
	}()

	startedAt = time.Now().Unix()
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		<-parseDone
		endedAt = time.Now().Unix()
		return result.Outcome{
			Status:   result.StatusFailure,
			ExitCode: result.ExitExecutionFailure,
			Error:    fmt.Sprintf("failed to start %s: %v", driver.Binary(), err),
		}
	}

	watcherCtx, stopWatcher := context.WithCancel(ctx)
	defer stopWatcher()
	timeoutReached := make(chan struct{}, 1)
	go watchActivity(watcherCtx, tracker, cfg.NoActivityTimeout, timeoutReached)

	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		// endedAt is written before the channel send; any later read
		// after <-done observes the write (channel-send happens-before
		// channel-receive).
		endedAt = time.Now().Unix()
		_ = pw.Close()
		<-parseDone
		done <- err
	}()

	select {
	case err := <-done:
		return buildOutcome(err, parser.State(), stderrBuf.String(), driver.Binary())
	case <-ctx.Done():
		return gracefulShutdown(cmd, done, parser, reasonSignal, cfg.NoActivityTimeout)
	case <-timeoutReached:
		fmt.Fprintf(os.Stderr, "[agentbox] no agent output for %s; killing subprocess\n", cfg.NoActivityTimeout)
		return gracefulShutdown(cmd, done, parser, reasonTimeout, cfg.NoActivityTimeout)
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
		PRTitle:        state.PRTitle,
		VerifyResult:   state.VerifyResult,
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
			PRTitle:        state.PRTitle,
			VerifyResult:   state.VerifyResult,
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
		PRTitle:        state.PRTitle,
		VerifyResult:   state.VerifyResult,
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

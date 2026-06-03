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
	"strconv"
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
	reasonLimit
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

	// Limit watcher: bounds turns / tokens for agents whose CLI lacks a
	// native cap (Codex). maxTurns reuses MAX_TURNS (the same value passed
	// to claude's --max-turns) as the numeric cap. Agents that report
	// turns/usage only at the end (claude) never trip it mid-run.
	limitReached := make(chan string, 1)
	maxTurnsLimit, _ := strconv.Atoi(cfg.MaxTurns)
	go watchLimits(watcherCtx, parser, maxTurnsLimit, cfg.TokenBudget, limitReached)

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
		return gracefulShutdown(cmd, done, parser, reasonSignal, "")
	case <-timeoutReached:
		detail := fmt.Sprintf("no agent output for %s; subprocess killed", cfg.NoActivityTimeout)
		fmt.Fprintf(os.Stderr, "[agentbox] %s\n", detail)
		return gracefulShutdown(cmd, done, parser, reasonTimeout, detail)
	case msg := <-limitReached:
		detail := msg + "; subprocess killed"
		fmt.Fprintf(os.Stderr, "[agentbox] %s\n", detail)
		return gracefulShutdown(cmd, done, parser, reasonLimit, detail)
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

func gracefulShutdown(cmd *exec.Cmd, done <-chan error, parser OutputParser, reason cancelReason, detail string) result.Outcome {
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
		base.Error = detail
	case reasonLimit:
		// A turn / token cap the agent's CLI didn't self-enforce — treated
		// as a failure (the Step didn't complete within budget), mirroring
		// claude's own error_max_turns being a failure.
		base.Status = result.StatusFailure
		base.ExitCode = result.ExitExecutionFailure
		base.Error = detail
	default:
		base.Status = result.StatusCancelled
		base.ExitCode = result.ExitCancelled
		base.Error = "cancelled by signal"
	}
	return base
}

// limitCheckInterval is how often the limit watcher samples the parser's
// progress. Coarse on purpose — turns and tokens move in chunks, and a few
// seconds of overshoot past the cap is acceptable for a backstop.
const limitCheckInterval = 3 * time.Second

// watchLimits polls the parser's accumulated turns / token usage and sends a
// reason on reached when a configured cap is exceeded, so the Run loop can
// shut the subprocess down. Agent-agnostic: agents whose parser surfaces
// these counters only at the very end (claude, which also self-limits via
// --max-turns) never trip it mid-run; agents that report incrementally
// (codex, which has no CLI cap) are bounded here. No-op when neither cap is
// set.
func watchLimits(ctx context.Context, parser OutputParser, maxTurns, tokenBudget int, reached chan<- string) {
	if maxTurns <= 0 && tokenBudget <= 0 {
		return
	}
	ticker := time.NewTicker(limitCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if msg, ok := limitExceeded(parser.State(), maxTurns, tokenBudget); ok {
				select {
				case reached <- msg:
				default:
				}
				return
			}
		}
	}
}

// limitExceeded reports whether the run has hit a configured turn or token
// cap, with a human-readable reason. Pure function — the watcher's decision
// logic, unit-tested without timers.
func limitExceeded(st ParsedState, maxTurns, tokenBudget int) (string, bool) {
	if maxTurns > 0 && st.Turns >= maxTurns {
		return fmt.Sprintf("reached its turn limit of %d", maxTurns), true
	}
	if tokenBudget > 0 {
		if used := st.TokenUsage.InputTokens + st.TokenUsage.OutputTokens; used >= tokenBudget {
			return fmt.Sprintf("reached its token budget of %d (used %d)", tokenBudget, used), true
		}
	}
	return "", false
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
		Error:          failureMessage(err, state, stderrText, binary),
		ChangesSummary: state.ChangesSummary,
		FilesChanged:   state.FilesChanged,
		TokenUsage:     state.TokenUsage,
		Turns:          state.Turns,
		PRTitle:        state.PRTitle,
		VerifyResult:   state.VerifyResult,
	}
}

// failureMessage builds the result.Error string, ordered most- to
// least-specific so the agent's own account of the failure always wins
// over a bare exit status. The raw "exit status N" branch is the last
// resort, reached only when the agent died without reporting anything.
func failureMessage(err error, state ParsedState, stderrText, binary string) string {
	switch {
	case state.FailureReason != "":
		// Tailored, actionable reason (e.g. hit max turns) — and the only
		// signal when the agent exits 0 yet reports is_error.
		return binary + " " + state.FailureReason
	case state.IsError && state.ChangesSummary != "":
		// The agent emitted a result event describing the failure in its
		// own words — prefer that over the exit status, which fires for
		// the same run and says nothing useful.
		return fmt.Sprintf("%s reported error: %s", binary, state.ChangesSummary)
	case state.IsError && state.ErrorSubtype != "":
		// Classified error with no description — at least name the class
		// (e.g. error_during_execution) so the subtype isn't lost.
		return fmt.Sprintf("%s reported error: %s", binary, state.ErrorSubtype)
	case state.IsError:
		return binary + " reported error"
	case err != nil && isExitError(err):
		// No result event at all: the agent died before reporting (crash,
		// early exit). "exit status 1" alone is useless, so append the
		// tail of its stderr as the best available detail.
		if tail := stderrTail(stderrText); tail != "" {
			return fmt.Sprintf("%s exited with error: %v — %s", binary, err, tail)
		}
		return fmt.Sprintf("%s exited with error: %v", binary, err)
	case err != nil:
		return fmt.Sprintf("failed to run %s: %v", binary, err)
	default:
		return binary + " reported error with no detail"
	}
}

func isExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// stderrTail returns the last non-empty line of the agent's stderr,
// trimmed and rune-capped, for embedding in a failure message. Returns
// "" when stderr held nothing useful. This is the only failure detail
// available when the agent dies before emitting a result event, so a
// noisy tail still beats a bare "exit status 1"; the full stderr is
// echoed to the container log regardless.
func stderrTail(s string) string {
	const maxRunes = 200
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > maxRunes {
			return string(r[:maxRunes]) + "…"
		}
		return line
	}
	return ""
}

package agent

import (
	"bytes"
	"context"
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

// This file defines the interactive-mode contract: the optional Driver
// capability for long-lived bidirectional sessions, the sink that receives
// structured output, the value types they exchange, and the RunInteractive
// process-lifecycle wrapper. Everything here stays agent-agnostic —
// per-agent wire protocols (Claude Code's stream-json, Codex's app-server
// JSON-RPC) live entirely behind a driver's RunSession.

// InteractiveDriver is the optional capability a Driver implements to
// support AGENT_MODE=interactive. RunInteractive type-asserts the resolved
// Driver to this interface; a Driver that doesn't implement it only runs in
// batch mode. The driver owns its wire protocol end to end (RunSession);
// RunInteractive only spawns the process and manages its lifecycle.
type InteractiveDriver interface {
	Driver

	// BuildInteractiveArgs returns the argv (after Binary()) that launches
	// the agent in its interactive/streaming mode.
	BuildInteractiveArgs(cfg *config.Config) []string

	// RunSession drives one interactive session over the spawned process's
	// pipes (sess.Stdin / sess.Stdout): it reads user turns from
	// sess.IO.NextUserMessage, writes its wire protocol to stdin, parses the
	// agent's stdout, and forwards assistant chunks / finals / specs to
	// sess.IO. It returns nil on a clean session end (input exhausted or
	// stdout EOF) and an error on a fatal protocol failure. The driver owns
	// stdin and closes it when it has no more to send. RunInteractive owns
	// process spawn/teardown; RunSession owns the protocol only.
	RunSession(ctx context.Context, cfg *config.Config, sess *InteractiveSession) error
}

// InteractiveSession is what RunInteractive hands the driver: the process
// pipes plus the session IO and a log sink.
type InteractiveSession struct {
	// Stdin is the agent process's stdin; the driver writes its wire
	// protocol here and closes it when it has no more to send.
	Stdin io.WriteCloser
	// Stdout is the agent process's stdout (already tee'd to the no-activity
	// watchdog). The driver reads and parses its wire protocol from here.
	Stdout io.Reader
	// IO is the session's user-input source and structured output sink.
	IO InteractiveIO
	// Log is where the driver writes human-readable log lines for the
	// container log; typically os.Stdout.
	Log io.Writer
}

// InteractiveSink receives structured updates parsed out of the agent's
// output stream by a driver's RunSession. The production implementation
// forwards onward to the runner (internal/interactive); tests collect into
// buffers. Calls arrive from the driver's single output-reader goroutine
// and are therefore serialized with respect to each other.
type InteractiveSink interface {
	// ForwardChunk delivers an incremental assistant-text delta (token-level
	// streaming). Many per turn.
	ForwardChunk(chunk AssistantChunk) error

	// ForwardFinal delivers the completed assistant message for a turn.
	ForwardFinal(msg AssistantMessage) error

	// ForwardSpecUpdate delivers a task-spec extracted from an assistant
	// message. Called only when the message carried a valid spec block.
	ForwardSpecUpdate(spec SpecSnapshot) error
}

// InteractiveIO is the full duplex a session drives: a sink for output plus
// a source of user input and a liveness heartbeat. The production
// implementation watches an input directory and writes to an output
// directory; tests supply channels.
type InteractiveIO interface {
	InteractiveSink

	// NextUserMessage blocks until the next user message is available or ctx
	// is cancelled (returns ctx.Err()). A clean end-of-input is signalled by
	// returning io.EOF.
	NextUserMessage(ctx context.Context) (UserMessage, error)

	// Heartbeat is called on a fixed interval with a liveness snapshot,
	// independent of agent output, so the runner can tell "agent thinking"
	// apart from "agent wedged". Best-effort.
	Heartbeat(state SessionState) error
}

// UserMessage is one inbound user turn.
type UserMessage struct {
	// ID identifies the message for de-duplication and correlation; assigned
	// by the IO source (e.g. the input filename).
	ID string
	// Text is the user's message body.
	Text string
}

// AssistantChunk is an incremental slice of assistant text within a turn.
// Tool-use, tool-result, and thinking events are surfaced to the human log
// by the driver but are not (yet) forwarded as chat chunks — only
// user-visible assistant text streams here.
type AssistantChunk struct {
	Text string
}

// AssistantMessage is a completed assistant turn's text, with any
// machine-only trailers (e.g. the task-spec block) already stripped.
type AssistantMessage struct {
	Text string
}

// SpecSnapshot is the structured task-spec the agent maintains across the
// conversation, parsed from a ```task-spec``` block in its output. It is
// the shape the resulting Task's description is built from. Held here as a
// pass-through DTO; parsing lives in internal/spec.
type SpecSnapshot struct {
	Title       string
	Goal        string
	Context     string
	Acceptance  []string
	Assumptions []string
	OutOfScope  []string
	Readiness   string // "vague" | "partial" | "ready"
	Notes       string
	// Complexity is the agent's hint for the EXECUTION task's model tier:
	// "low" | "medium" | "high". Advisory — the convert step maps it to a
	// default model within the chosen agent; the user can override.
	Complexity string
	// Raw is the verbatim JSON payload of the block, for storage/audit.
	Raw string
}

// SessionState is the periodic liveness snapshot passed to Heartbeat.
type SessionState struct {
	Turns        int
	InputTokens  int
	OutputTokens int
}

// HeartbeatInterval is the default cadence drivers use with RunHeartbeat.
const HeartbeatInterval = 30 * time.Second

// RunInteractive spawns the agent in interactive mode and hands its pipes to
// the driver's RunSession, which owns the wire protocol. RunInteractive owns
// only the process lifecycle: it tees stdout to the no-activity watchdog,
// and terminates on a clean session end, the agent exiting, ctx cancellation
// (SIGTERM from the runner's stop signal), or the no-activity timeout —
// SIGTERM with a grace period, then SIGKILL.
//
// driver must implement InteractiveDriver; otherwise the call fails fast.
func RunInteractive(ctx context.Context, cfg *config.Config, driver Driver, iio InteractiveIO) (outcome result.Outcome) {
	idriver, ok := driver.(InteractiveDriver)
	if !ok {
		return result.Outcome{
			Status:   result.StatusFailure,
			ExitCode: result.ExitExecutionFailure,
			Error:    fmt.Sprintf("agent %q does not support interactive mode", cfg.AgentType),
		}
	}

	agentVersion := driver.DetectVersion()

	cmd := exec.Command(driver.Binary(), idriver.BuildInteractiveArgs(cfg)...)
	cmd.Dir = cfg.WorkDir
	cmd.Env = buildEnv()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return interactiveStartFailure(driver.Binary(), "stdin", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return interactiveStartFailure(driver.Binary(), "stdout", err)
	}

	tracker := newActivityTracker()
	stderrBuf := &bytes.Buffer{}
	cmd.Stderr = io.MultiWriter(os.Stderr, stderrBuf, tracker)

	var startedAt, endedAt int64
	defer func() {
		outcome.AgentType = cfg.AgentType
		outcome.AgentVersion = agentVersion
		outcome.StartedAt = startedAt
		outcome.EndedAt = endedAt
		if outcome.Model == "" {
			outcome.Model = cfg.Model
		}
	}()

	startedAt = time.Now().Unix()
	if err := cmd.Start(); err != nil {
		endedAt = time.Now().Unix()
		return interactiveStartFailure(driver.Binary(), "start", err)
	}

	// The driver owns the wire protocol. It reads stdout (tee'd to the
	// activity tracker so the no-output watchdog still works) and writes its
	// protocol to stdin, closing stdin when done sending.
	sess := &InteractiveSession{
		Stdin:  stdin,
		Stdout: io.TeeReader(stdoutPipe, tracker),
		IO:     iio,
		Log:    os.Stdout,
	}
	sessCtx, stopSession := context.WithCancel(ctx)
	defer stopSession()
	sessionDone := make(chan error, 1)
	go func() { sessionDone <- idriver.RunSession(sessCtx, cfg, sess) }()

	watcherCtx, stopWatcher := context.WithCancel(ctx)
	defer stopWatcher()
	timeoutReached := make(chan struct{}, 1)
	go watchActivity(watcherCtx, tracker, cfg.NoActivityTimeout, timeoutReached)

	done := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		endedAt = time.Now().Unix()
		done <- waitErr
	}()

	select {
	case waitErr := <-done:
		// Agent exited on its own (session ended and stdin closed, budget
		// self-exit, or crash). Let the driver's reader drain to EOF.
		stopSession()
		drain(sessionDone)
		return interactiveExitOutcome(waitErr, stderrBuf.String(), driver.Binary())
	case serr := <-sessionDone:
		// Driver finished or hit a fatal protocol error; wind the process
		// down. The outcome reflects the driver's view, not the SIGTERM kill.
		stopSession()
		_ = terminate(cmd, done)
		if ctx.Err() != nil {
			// A cancellation that surfaced as a session error is still a
			// cancellation, not a failure.
			return result.Outcome{Status: result.StatusCancelled, ExitCode: result.ExitCancelled, Error: "cancelled by signal"}
		}
		return sessionEndOutcome(serr)
	case <-ctx.Done():
		stopSession()
		_ = terminate(cmd, done)
		drain(sessionDone)
		return result.Outcome{Status: result.StatusCancelled, ExitCode: result.ExitCancelled, Error: "cancelled by signal"}
	case <-timeoutReached:
		detail := fmt.Sprintf("no agent output for %s; interactive session ended", cfg.NoActivityTimeout)
		fmt.Fprintf(os.Stderr, "[agentbox] %s\n", detail)
		stopSession()
		_ = terminate(cmd, done)
		drain(sessionDone)
		return result.Outcome{Status: result.StatusTimeout, ExitCode: result.ExitTimeout, Error: detail}
	}
}

// terminate sends SIGTERM, waits up to shutdownGrace for the process to
// exit, then SIGKILLs. Returns the process wait error.
func terminate(cmd *exec.Cmd, done <-chan error) error {
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(shutdownGrace):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return <-done
	}
}

// drain waits briefly for the driver's RunSession goroutine to return so its
// forwarding finishes before RunInteractive does, without hanging if the
// driver is wedged.
func drain(sessionDone <-chan error) {
	select {
	case <-sessionDone:
	case <-time.After(2 * time.Second):
	}
}

func interactiveStartFailure(binary, what string, err error) result.Outcome {
	return result.Outcome{
		Status:   result.StatusFailure,
		ExitCode: result.ExitExecutionFailure,
		Error:    fmt.Sprintf("failed to open %s for %s: %v", what, binary, err),
	}
}

// interactiveExitOutcome maps an agent process exit to an Outcome (used when
// the process exits before the driver returns).
func interactiveExitOutcome(waitErr error, stderrText, binary string) result.Outcome {
	if waitErr == nil {
		return result.Outcome{Status: result.StatusSuccess, ExitCode: result.ExitSuccess}
	}
	exit := result.ExitExecutionFailure
	if HasAuthKeyword(strings.ToLower(stderrText)) {
		exit = result.ExitAuthFailure
	}
	msg := fmt.Sprintf("%s exited with error: %v", binary, waitErr)
	if tail := stderrTail(stderrText); tail != "" {
		msg += " — " + tail
	}
	return result.Outcome{Status: result.StatusFailure, ExitCode: exit, Error: msg}
}

// sessionEndOutcome maps a driver RunSession return to an Outcome. A nil
// error is a clean session end (success); a non-nil error is a protocol
// failure. The process's own exit status is ignored here because we SIGTERM
// it after the session ends.
func sessionEndOutcome(serr error) result.Outcome {
	if serr != nil {
		return result.Outcome{Status: result.StatusFailure, ExitCode: result.ExitExecutionFailure, Error: serr.Error()}
	}
	return result.Outcome{Status: result.StatusSuccess, ExitCode: result.ExitSuccess}
}

// PumpTextStdin feeds user turns to a simple line-oriented agent: for each
// message from iio it writes encode(text) to stdin, until ctx is cancelled
// or iio returns an error (e.g. io.EOF). It owns stdin and closes it on
// return. Drivers whose protocol is one self-contained line per user turn
// (Claude Code's stream-json) use this; stateful protocols (Codex's
// JSON-RPC) drive stdin themselves.
func PumpTextStdin(ctx context.Context, iio InteractiveIO, encode func(string) ([]byte, error), stdin io.WriteCloser) {
	defer stdin.Close()
	for {
		msg, err := iio.NextUserMessage(ctx)
		if err != nil {
			return // ctx cancelled or end-of-input
		}
		line, err := encode(msg.Text)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[agentbox] encode user message %s: %v\n", msg.ID, err)
			continue
		}
		if _, err := stdin.Write(line); err != nil {
			fmt.Fprintf(os.Stderr, "[agentbox] stdin write failed: %v\n", err)
			return
		}
	}
}

// RunHeartbeat reports a liveness snapshot to iio every interval until ctx is
// cancelled. stats supplies the current snapshot (drivers close over their
// own accumulated counters).
func RunHeartbeat(ctx context.Context, interval time.Duration, iio InteractiveIO, stats func() SessionState) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = iio.Heartbeat(stats())
		}
	}
}

package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
)

// This file defines the interactive-mode contract: the optional Driver
// capability for long-lived bidirectional sessions, the sink that
// receives structured output, and the value types they exchange. The
// orchestration loop itself (RunInteractive) lives alongside in this
// package. Everything here stays agent-agnostic — stream-json and other
// format specifics live in the agent packages (e.g. internal/claude).

// InteractiveDriver is the optional capability a Driver implements to
// support AGENT_MODE=interactive. RunInteractive type-asserts the
// resolved Driver to this interface; a Driver that doesn't implement it
// only runs in batch mode. Kept separate from Driver so batch-only
// agents and the batch path are unaffected (per the small-interfaces
// convention in this package).
type InteractiveDriver interface {
	Driver

	// BuildInteractiveArgs returns the argv (after Binary()) that
	// launches the agent in streaming bidirectional mode: user turns are
	// fed as line-delimited stream-json on stdin, and the agent emits its
	// events as stream-json on stdout.
	BuildInteractiveArgs(cfg *config.Config) []string

	// EncodeUserMessage renders one user turn as the stream-json stdin
	// envelope the agent expects, including the trailing newline. Called
	// once per user message by the input pump.
	EncodeUserMessage(text string) ([]byte, error)

	// NewChunkForwarder returns a WriteCloser that consumes the agent's
	// raw stdout and forwards structured updates to sink as they arrive.
	// Mirrors NewLogFormatter: the driver owns all output-format
	// knowledge so this package stays agent-agnostic. Close flushes any
	// buffered partial line.
	NewChunkForwarder(sink InteractiveSink) io.WriteCloser
}

// InteractiveSink receives structured updates parsed out of the agent's
// output stream. NewChunkForwarder writes to it; the production
// implementation forwards onward to the runner (internal/interactive),
// while tests collect into buffers.
//
// Calls arrive from the single stdout-reader goroutine and are therefore
// serialized with respect to each other.
type InteractiveSink interface {
	// ForwardChunk delivers an incremental assistant-text delta, emitted
	// when the agent runs with token-level partial messages. Many per
	// turn.
	ForwardChunk(chunk AssistantChunk) error

	// ForwardFinal delivers the completed assistant message for a turn.
	ForwardFinal(msg AssistantMessage) error

	// ForwardSpecUpdate delivers a task-spec extracted from an assistant
	// message. Called only when the message carried a valid spec block.
	ForwardSpecUpdate(spec SpecSnapshot) error
}

// InteractiveIO is the full duplex RunInteractive drives: a sink for
// output plus a source of user input and a liveness heartbeat. The
// production implementation watches an input directory and writes to an
// output directory; tests supply channels.
type InteractiveIO interface {
	InteractiveSink

	// NextUserMessage blocks until the next user message is available or
	// ctx is cancelled (in which case it returns ctx.Err()). A clean
	// end-of-input is signalled by returning io.EOF.
	NextUserMessage(ctx context.Context) (UserMessage, error)

	// Heartbeat is called by RunInteractive on a fixed interval with a
	// liveness snapshot, independent of agent output, so the runner can
	// tell "agent thinking" apart from "agent wedged". Best-effort.
	Heartbeat(state SessionState) error
}

// UserMessage is one inbound user turn.
type UserMessage struct {
	// ID identifies the message for de-duplication and correlation;
	// assigned by the IO source (e.g. the input filename).
	ID string
	// Text is the user's message body.
	Text string
}

// AssistantChunk is an incremental slice of assistant text within a turn.
// Tool-use, tool-result, and thinking events are summarized to the human
// log by the driver's formatter but are not (yet) forwarded as chat
// chunks — only user-visible assistant text streams here.
type AssistantChunk struct {
	Text string
}

// AssistantMessage is a completed assistant turn's text, with any
// machine-only trailers (e.g. the task-spec block) already stripped by
// the forwarder.
type AssistantMessage struct {
	Text string
}

// SpecSnapshot is the structured task-spec the agent maintains across the
// conversation, parsed from a ```task-spec``` block in its output. It is
// the shape the resulting Task's description is built from. Held here as
// a pass-through DTO; parsing lives in internal/spec.
type SpecSnapshot struct {
	Title       string
	Goal        string
	Context     string
	Acceptance  []string
	Assumptions []string
	OutOfScope  []string
	Readiness   string // "vague" | "partial" | "ready"
	Notes       string
	// Raw is the verbatim JSON payload of the block, for storage/audit.
	Raw string
}

// SessionState is the periodic liveness snapshot passed to Heartbeat.
type SessionState struct {
	Turns        int
	InputTokens  int
	OutputTokens int
}

// heartbeatInterval is how often RunInteractive reports liveness to the
// InteractiveIO, independent of agent output, so the runner can tell an
// agent that's thinking from one that's wedged.
const heartbeatInterval = 30 * time.Second

// RunInteractive drives a long-lived bidirectional session: it feeds user
// turns from iio to the agent's stdin and forwards the agent's streamed
// output back through iio, until the agent exits, ctx is cancelled
// (SIGTERM from the runner's stop signal), or the no-activity watchdog
// fires. It mirrors Run's process lifecycle (graceful SIGTERM→SIGKILL,
// stats parsing, self-describing Outcome) but adds the stdin pump, the
// structured chunk forwarder, and a periodic heartbeat.
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
		return result.Outcome{
			Status:   result.StatusFailure,
			ExitCode: result.ExitExecutionFailure,
			Error:    fmt.Sprintf("failed to open stdin for %s: %v", driver.Binary(), err),
		}
	}

	parser := driver.NewOutputParser()
	pr, pw := io.Pipe()
	tracker := newActivityTracker()

	humanOut := driver.NewLogFormatter(os.Stdout)
	defer humanOut.Close()

	forwarder := idriver.NewChunkForwarder(iio)
	defer forwarder.Close()

	// stdout fans out to: the human container log, the structured chunk
	// forwarder (chat stream + spec), the stats parser (for the final
	// result), and the activity tracker.
	cmd.Stdout = io.MultiWriter(humanOut, forwarder, pw, tracker)
	stderrBuf := &bytes.Buffer{}
	cmd.Stderr = io.MultiWriter(os.Stderr, stderrBuf, tracker)

	parseDone := make(chan struct{})
	go func() {
		defer close(parseDone)
		parser.Consume(pr)
	}()

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

	// The input pump owns stdin and closes it when it stops (ctx cancelled
	// or the source signals EOF). Closing stdin tells the agent no more
	// turns are coming, so it finishes cleanly.
	pumpCtx, stopPump := context.WithCancel(ctx)
	defer stopPump()
	go pumpUserMessages(pumpCtx, idriver.EncodeUserMessage, iio, stdin)

	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go heartbeatLoop(hbCtx, heartbeatInterval, parser, iio)

	watcherCtx, stopWatcher := context.WithCancel(ctx)
	defer stopWatcher()
	timeoutReached := make(chan struct{}, 1)
	go watchActivity(watcherCtx, tracker, cfg.NoActivityTimeout, timeoutReached)

	done := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		endedAt = time.Now().Unix()
		_ = pw.Close()
		<-parseDone
		done <- waitErr
	}()

	select {
	case waitErr := <-done:
		return buildOutcome(waitErr, parser.State(), stderrBuf.String(), driver.Binary())
	case <-ctx.Done():
		// Stop the pump so stdin closes (graceful "no more input"); the
		// SIGTERM in gracefulShutdown is the backstop.
		stopPump()
		return gracefulShutdown(cmd, done, parser, reasonSignal, "")
	case <-timeoutReached:
		detail := fmt.Sprintf("no agent output for %s; interactive session ended", cfg.NoActivityTimeout)
		fmt.Fprintf(os.Stderr, "[agentbox] %s\n", detail)
		stopPump()
		return gracefulShutdown(cmd, done, parser, reasonTimeout, detail)
	}
}

// pumpUserMessages feeds user turns to the agent's stdin until ctx is
// cancelled or the source returns an error (e.g. io.EOF for clean
// end-of-input). It owns stdin and closes it on return. encode renders a
// turn into the agent's stdin envelope (idriver.EncodeUserMessage in
// production; a stub in tests).
func pumpUserMessages(ctx context.Context, encode func(string) ([]byte, error), iio InteractiveIO, stdin io.WriteCloser) {
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

// heartbeatLoop reports a liveness snapshot to iio every interval until
// ctx is cancelled. interval is a parameter (not the const) so tests can
// drive it fast.
func heartbeatLoop(ctx context.Context, interval time.Duration, parser OutputParser, iio InteractiveIO) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st := parser.State()
			_ = iio.Heartbeat(SessionState{
				Turns:        st.Turns,
				InputTokens:  st.TokenUsage.InputTokens,
				OutputTokens: st.TokenUsage.OutputTokens,
			})
		}
	}
}

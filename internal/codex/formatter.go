package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

// rawStreamLogPath captures Codex's unfiltered `exec --json` stream for deep
// debugging. The Dockerfile pre-creates /scratch writable by the agent user;
// outside the container the open is expected to fail, in which case raw
// teeing is silently skipped (the human summary still goes to the sink).
const rawStreamLogPath = "/scratch/agent.log"

// Per-line width budget — the dashboard's log viewer renders fixed-width and
// wraps awkwardly past ~200 chars. Aggressive truncation keeps the viewport
// readable; the full payload stays on /scratch/agent.log.
const (
	maxLineWidth   = 200
	textExcerptLen = 200
	cmdExcerptLen  = 160
)

// openRawStreamLog opens the raw-stream tee, returning nil when unavailable
// (e.g. running outside the container). A nil rawSink disables teeing.
func openRawStreamLog() io.WriteCloser {
	f, err := os.OpenFile(rawStreamLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil
	}
	return f
}

// humanLogFormatter wraps a sink io.Writer with a line-buffered translator
// that turns Codex's `exec --json` events into compact summary lines
// (`▶ session`, `$ <cmd>`, `✎ <files>`, `🗨 <message>`, `✓ turn complete`).
// Bytes that don't parse as a codex event (npm install output, proxy deny
// logs, Node stack traces) pass through verbatim — they're already legible
// and often the important part of an incident. Mirrors the claude driver's
// formatter shape.
//
// rawSink, when non-nil, receives every byte before line-splitting so the
// unfiltered JSONL stays available for debugging.
//
// Concurrency: cmd.Stdout is driven from a single os/exec goroutine, so
// Write() is effectively single-threaded; the mutex guards Close() against a
// late flush and keeps the buffer consistent.
type humanLogFormatter struct {
	sink    io.Writer
	rawSink io.WriteCloser

	mu  sync.Mutex
	buf bytes.Buffer
}

func newHumanLogFormatter(sink io.Writer, rawSink io.WriteCloser) *humanLogFormatter {
	return &humanLogFormatter{sink: sink, rawSink: rawSink}
}

// Write buffers incoming bytes, splits on newline, and emits one summary line
// per logical event. Sink write errors are swallowed — a failed log write
// must not stall the agent run (same posture as the prior passthrough).
func (f *humanLogFormatter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.rawSink != nil {
		_, _ = f.rawSink.Write(p)
	}

	f.buf.Write(p)
	for {
		b := f.buf.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		line := append([]byte(nil), b[:i]...)
		f.buf.Next(i + 1)
		f.emitLine(line)
	}
	return len(p), nil
}

// Close flushes any pending partial line, then closes rawSink if owned. Safe
// to call multiple times.
func (f *humanLogFormatter) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buf.Len() > 0 {
		line := append([]byte(nil), f.buf.Bytes()...)
		f.buf.Reset()
		f.emitLine(line)
	}
	var err error
	if f.rawSink != nil {
		err = f.rawSink.Close()
		f.rawSink = nil
	}
	return err
}

func (f *humanLogFormatter) emitLine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return
	}
	// Cheap early-out: lines that don't start with '{' aren't codex events.
	// Forwarding them verbatim preserves proxy/npm/Node output that's already
	// human-readable.
	if trimmed[0] != '{' {
		f.passthrough(line)
		return
	}
	summary, ok := formatCodexJSONLine(trimmed)
	if !ok {
		f.passthrough(line)
		return
	}
	if summary == "" {
		return
	}
	_, _ = io.WriteString(f.sink, truncate(summary, maxLineWidth)+"\n")
}

func (f *humanLogFormatter) passthrough(line []byte) {
	_, _ = f.sink.Write(append(bytes.TrimRight(line, "\r\n"), '\n'))
}

// formatCodexJSONLine maps one codex JSONL event to a compact human line.
// ("", true) deliberately drops a recognized-but-noisy event (turn.started,
// unknown types). (_, false) means the line isn't a codex event at all, so
// the caller passes it through verbatim.
func formatCodexJSONLine(line []byte) (string, bool) {
	var ev codexEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return "", false
	}
	switch ev.Type {
	case "thread.started":
		return "▶ codex session started", true
	case "turn.completed":
		if ev.Usage != nil {
			return fmt.Sprintf("✓ turn complete · in %d / out %d / cached %d",
				ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CachedInputTokens), true
		}
		return "✓ turn complete", true
	case "turn.failed", "error":
		msg := ev.failureMessage()
		if msg == "" {
			msg = "unknown error"
		}
		return "✗ " + oneLine(msg, textExcerptLen), true
	case "item.started", "item.completed":
		return formatCodexItem(ev.Type == "item.completed", ev.Item), true
	}
	return "", true // turn.started and unknown event types: drop quietly
}

// formatCodexItem renders an item.started / item.completed payload. Returning
// "" drops the event (e.g. the duplicate item.started for a message we only
// want to show once it's complete, or a clean command exit already announced
// on start).
func formatCodexItem(completed bool, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var it codexItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return ""
	}
	switch it.Type {
	case "command_execution":
		if !completed {
			return "$ " + oneLine(it.Command, cmdExcerptLen)
		}
		if it.ExitCode != nil && *it.ExitCode != 0 {
			// Repeat the command on failure so the line is self-contained
			// even if the start line scrolled away (or never fired).
			if cmd := oneLine(it.Command, cmdExcerptLen); cmd != "" {
				return fmt.Sprintf("✗ exit %d · %s", *it.ExitCode, cmd)
			}
			return fmt.Sprintf("✗ exit %d", *it.ExitCode)
		}
		return "" // clean exit — the command was already shown on start
	case "file_change":
		if !completed {
			return ""
		}
		parts := make([]string, 0, len(it.Changes))
		for _, ch := range it.Changes {
			if ch.Path != "" {
				parts = append(parts, strings.TrimSpace(changeMark(ch.Kind)+" "+ch.Path))
			}
		}
		if len(parts) == 0 {
			return "✎ file change"
		}
		return "✎ " + strings.Join(parts, ", ")
	case "agent_message":
		if !completed { // avoid the started/completed duplicate
			return ""
		}
		return "🗨 " + oneLine(it.Text, textExcerptLen)
	case "reasoning":
		if !completed {
			return ""
		}
		return "… " + oneLine(it.Text, textExcerptLen)
	}
	return ""
}

// changeMark maps a file_change kind to a one-char diff-style glyph.
func changeMark(kind string) string {
	switch kind {
	case "add", "added", "create", "created":
		return "+"
	case "delete", "deleted", "remove", "removed":
		return "-"
	default:
		return "~" // update / rename / unknown
	}
}

// oneLine collapses internal whitespace and newlines to single spaces, then
// caps the result to n runes.
func oneLine(s string, n int) string {
	return truncate(strings.Join(strings.Fields(s), " "), n)
}

// truncate caps s to n runes (rune-safe), appending an ellipsis when cut.
func truncate(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

package opencode

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

// rawStreamLogPath captures opencode's unfiltered `run --format json` stream for
// deep debugging. The Dockerfile pre-creates /scratch writable by the agent
// user; outside the container the open is expected to fail, in which case raw
// teeing is silently skipped (the human summary still goes to the sink). Only
// one agent runs per container, so sharing the path with the codex driver is
// fine.
const rawStreamLogPath = "/scratch/agent.log"

// Per-line width budget — the dashboard's log viewer renders fixed-width and
// wraps awkwardly past ~200 chars. The full payload stays on /scratch/agent.log.
const (
	maxLineWidth   = 200
	textExcerptLen = 200
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

// humanLogFormatter wraps a sink io.Writer with a line-buffered translator that
// turns opencode's `run --format json` events into compact summary lines. Bytes
// that don't parse as an opencode event (npm install output, proxy deny logs,
// Node stack traces) pass through verbatim. Mirrors the codex driver's
// formatter shape.
//
// rawSink, when non-nil, receives every byte before line-splitting so the
// unfiltered JSONL stays available for debugging.
//
// Concurrency: cmd.Stdout is driven from a single os/exec goroutine, so Write()
// is effectively single-threaded; the mutex guards Close() against a late flush
// and keeps the buffer consistent.
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
// per logical event. Sink write errors are swallowed — a failed log write must
// not stall the agent run.
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

// Close flushes any pending partial line, then closes rawSink if owned. Safe to
// call multiple times.
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
	// Cheap early-out: lines that don't start with '{' aren't opencode events.
	if trimmed[0] != '{' {
		f.passthrough(line)
		return
	}
	summary, ok := formatOpencodeJSONLine(trimmed)
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

// formatOpencodeJSONLine maps one opencode JSONL event to a compact human line.
// ("", true) deliberately drops a recognized-but-noisy event. (_, false) means
// the line isn't an opencode event at all, so the caller passes it through.
func formatOpencodeJSONLine(line []byte) (string, bool) {
	var ev opencodeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return "", false
	}
	switch ev.Type {
	case "text":
		var part opencodePart
		if json.Unmarshal(ev.Part, &part) == nil && strings.TrimSpace(part.Text) != "" {
			return "🗨 " + oneLine(part.Text, textExcerptLen), true
		}
		return "", true
	case "step_finish", "step.finish":
		var part opencodePart
		if json.Unmarshal(ev.Part, &part) == nil && part.Tokens != nil {
			return fmt.Sprintf("✓ step complete · in %d / out %d / cache %d",
				part.Tokens.Input, part.Tokens.Output, part.Tokens.Cache.Read), true
		}
		return "✓ step complete", true
	case "tool", "tool_use":
		var part opencodePart
		if json.Unmarshal(ev.Part, &part) == nil && part.Tool != "" {
			return "⚙ " + oneLine(part.Tool, textExcerptLen), true
		}
		return "", true
	case "error":
		return "✗ " + oneLine(ev.errorMessage(), textExcerptLen), true
	}
	return "", true // unknown event types: drop quietly
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

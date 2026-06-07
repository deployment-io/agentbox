package claude

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/spec"
)

// Compile-time check: the Claude driver implements the full interactive
// capability now that args, encoder, and forwarder all exist.
var _ agent.InteractiveDriver = (*Driver)(nil)

// NewChunkForwarder returns a WriteCloser that consumes Claude Code's
// stream-json stdout line-by-line and forwards structured updates to
// sink: assistant text (streamed token-by-token when partial-message
// deltas are present, otherwise one chunk per completed message), the
// completed message per turn, and any ```task-spec``` block. Tool-use,
// tool-result, thinking, and init events are not chat-visible; the
// container's human log (NewLogFormatter) surfaces those.
func (d *Driver) NewChunkForwarder(sink agent.InteractiveSink) io.WriteCloser {
	return &chunkForwarder{sink: sink}
}

type chunkForwarder struct {
	sink agent.InteractiveSink

	mu  sync.Mutex
	buf bytes.Buffer
	// streamedThisMsg records whether partial deltas were forwarded for
	// the in-progress assistant message, so the completed message isn't
	// re-sent as a duplicate chunk.
	streamedThisMsg bool
}

// Write buffers bytes, splits on newline, and handles one stream-json
// event per line. Mirrors humanLogFormatter.Write so a line split across
// Write calls is reassembled before parsing.
func (f *chunkForwarder) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf.Write(p)
	for {
		b := f.buf.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		line := append([]byte(nil), b[:i]...)
		f.buf.Next(i + 1)
		f.handleLine(line)
	}
	return len(p), nil
}

// Close flushes any trailing partial line (a final event not terminated
// by a newline). Safe to call once.
func (f *chunkForwarder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buf.Len() > 0 {
		line := append([]byte(nil), f.buf.Bytes()...)
		f.buf.Reset()
		f.handleLine(line)
	}
	return nil
}

type forwardEvent struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
	Result  string          `json:"result"`
	Event   *partialEvent   `json:"event"`
}

type partialEvent struct {
	Delta *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

func (f *chunkForwarder) handleLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var ev forwardEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "assistant":
		f.handleAssistant(ev.Message)
	case "system", "user", "result":
		// init / tool_result / end-of-turn result: not chat-visible here.
	default:
		// Best-effort token streaming. Partial-message events are emitted
		// only with --include-partial-messages and their wrapper type is
		// outside the documented contract, so match on the delta shape and
		// ignore anything unrecognized — the completed "assistant" event
		// still carries the full text.
		if ev.Event != nil && ev.Event.Delta != nil && ev.Event.Delta.Text != "" {
			f.streamedThisMsg = true
			_ = f.sink.ForwardChunk(agent.AssistantChunk{Text: ev.Event.Delta.Text})
		}
	}
}

// handleAssistant processes a completed assistant message: forwards any
// task-spec block, then the user-visible text (as one chunk if it wasn't
// already streamed via partial deltas) and the turn-final message. The
// spec block is parsed from the full text but stripped from the chat text.
func (f *chunkForwarder) handleAssistant(raw json.RawMessage) {
	full := assistantText(raw)
	streamed := f.streamedThisMsg
	f.streamedThisMsg = false

	if s, ok := spec.Extract(full); ok {
		_ = f.sink.ForwardSpecUpdate(s)
	}

	display := spec.Strip(full)
	if display == "" {
		return // nothing user-visible (e.g. a tool-only or spec-only message)
	}
	if !streamed {
		_ = f.sink.ForwardChunk(agent.AssistantChunk{Text: display})
	}
	_ = f.sink.ForwardFinal(agent.AssistantMessage{Text: display})
}

// assistantText concatenates the text content blocks of an assistant
// message envelope, ignoring thinking / tool_use / tool_result blocks.
// Reuses the formatter's message types (same package).
func assistantText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var msg humanMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range msg.Content {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

package claude

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/reposuggestion"
	"github.com/deployment-io/agentbox/internal/spec"
)

// chunkForwarder parses Claude Code's stream-json output line-by-line and
// forwards structured updates to a sink: assistant text (streamed
// token-by-token when partial-message deltas are present, otherwise one
// chunk per completed message), the completed message per turn, and any
// ```task-spec``` or <repo-suggestion> block. Tool-use, tool-result,
// thinking, and init events are not chat-visible; the container's human log
// surfaces those.
//
// handleLine is called from the session's single stdout-reader goroutine,
// so the forwarder needs no internal locking.
type chunkForwarder struct {
	sink agent.InteractiveSink
	// streamedThisMsg records whether partial deltas were forwarded for the
	// in-progress assistant message, so the completed message isn't re-sent
	// as a duplicate chunk.
	streamedThisMsg bool
	// lastFinal is the turn's most recent chat-visible message. Claude Code
	// reports some failures (API errors) as an assistant message AND repeats
	// the same text on the failed result; remembering it keeps the failure
	// explanation from quoting what the user has just read.
	lastFinal string
}

func newChunkForwarder(sink agent.InteractiveSink) *chunkForwarder {
	return &chunkForwarder{sink: sink}
}

type forwardEvent struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
	Result  string          `json:"result"`
	Event   *partialEvent   `json:"event"`
	// Subtype / IsError / NumTurns classify a result event: "success" (or
	// absent, on older CLIs) for a finished turn, else the failure class —
	// error_max_turns, error_max_budget_usd, error_during_execution, ...
	Subtype  string `json:"subtype"`
	IsError  bool   `json:"is_error"`
	NumTurns int    `json:"num_turns"`
}

type partialEvent struct {
	Delta *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

// handleLine processes one stream-json line.
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
	case "result":
		f.handleResult(ev)
	case "system", "user":
		// init / tool_result: not chat-visible here.
	default:
		// Best-effort token streaming (--include-partial-messages). The
		// wrapper type is outside the documented contract, so match on the
		// delta shape and ignore anything unrecognized — the completed
		// "assistant" event still carries the full text.
		if ev.Event != nil && ev.Event.Delta != nil && ev.Event.Delta.Text != "" {
			f.streamedThisMsg = true
			_ = f.sink.ForwardChunk(agent.AssistantChunk{Text: ev.Event.Delta.Text})
		}
	}
}

// handleResult processes the end-of-turn result event: the agent is now
// blocked on user input. A successful result's text isn't chat-visible (it
// duplicates the last assistant message), but the boundary itself is
// forwarded — consumers gate their composer on it and group the turn's
// messages.
//
// A FAILED result is different: nothing else in the stream tells the user
// why the composer re-opened with no answer (the human log has the subtype,
// the chat does not), so the failure is forwarded first as an assistant-
// visible message that names the reason, then the boundary.
func (f *chunkForwarder) handleResult(ev forwardEvent) {
	f.streamedThisMsg = false
	shown := f.lastFinal
	f.lastFinal = ""
	if msg := turnFailureMessage(ev, shown); msg != "" {
		_ = f.sink.ForwardChunk(agent.AssistantChunk{Text: msg})
		_ = f.sink.ForwardFinal(agent.AssistantMessage{Text: msg})
	}
	_ = f.sink.ForwardTurnEnd()
}

// turnFailureMessage renders a failed result event as the one-line
// explanation the user sees in chat, or "" for a successful turn. The
// subtype → reason mapping is the batch parser's (resultFailurePredicate);
// only the remedy differs — in a session the user continues by sending a
// message, not by raising a cap. Subtypes the mapping doesn't cover fall
// back to naming the class plus an excerpt of the result text, which for
// error_during_execution is the agent's own description of what broke. The
// excerpt is dropped when it repeats alreadyShown, the message the user has
// just read.
func turnFailureMessage(ev forwardEvent, alreadyShown string) string {
	if !ev.IsError && (ev.Subtype == "" || ev.Subtype == "success") {
		return ""
	}
	predicate, capped := resultFailurePredicate(ev.Subtype, ev.NumTurns, ev.Result)
	switch {
	case capped && ev.Subtype == resultSubtypeMaxTurns:
		return "The agent " + predicate + "; send a message to continue."
	case predicate != "":
		return "The agent " + predicate + "."
	}
	msg := "The agent's turn failed"
	if ev.Subtype != "" && ev.Subtype != "success" {
		msg += " (" + ev.Subtype + ")"
	}
	excerpt := ellipsisOneLine(ev.Result, resultExcerptLen)
	if excerpt != "" && excerpt != ellipsisOneLine(alreadyShown, resultExcerptLen) {
		return msg + ": " + excerpt
	}
	return msg + "."
}

// handleAssistant processes a completed assistant message: forwards any
// task-spec and repo-suggestion block, then the user-visible text (as one chunk
// if it wasn't already streamed via partial deltas) and the turn-final message.
// Both blocks are parsed from the full text but stripped from the chat text; a
// message can carry either, both, or neither.
func (f *chunkForwarder) handleAssistant(raw json.RawMessage) {
	full := assistantText(raw)
	streamed := f.streamedThisMsg
	f.streamedThisMsg = false

	if s, ok := spec.Extract(full); ok {
		_ = f.sink.ForwardSpecUpdate(s)
	}
	if rs, ok := reposuggestion.Extract(full); ok {
		_ = f.sink.ForwardRepoSuggestion(rs)
	}

	display := reposuggestion.Strip(spec.Strip(full))
	if display == "" {
		return // nothing user-visible (e.g. a tool-only or block-only message)
	}
	if !streamed {
		_ = f.sink.ForwardChunk(agent.AssistantChunk{Text: display})
	}
	_ = f.sink.ForwardFinal(agent.AssistantMessage{Text: display})
	f.lastFinal = display
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

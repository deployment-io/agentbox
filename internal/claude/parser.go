package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/result"
)

// prTitleRe matches the <pr_title>...</pr_title> trailer the agent's
// system prompt instructs it to emit at the end of its final message.
// Dotall so a stray newline inside the tag is tolerated; non-greedy so
// only the first balanced pair is captured if the agent somehow emits
// more than one (shouldn't happen, but cheap insurance).
var prTitleRe = regexp.MustCompile(`(?s)<pr_title>(.*?)</pr_title>`)

// streamParser consumes Claude Code's --output-format=stream-json events
// line-by-line. Malformed or unknown events are silently skipped so one
// bad line doesn't drop the rest of the stream.
//
// State() is safe to call concurrently with Consume() — needed by the
// progress writer (Phase 5.5b) which snapshots the parser every few
// seconds while Consume is still running. mu guards every field
// mutated by processLine and read by State.
type streamParser struct {
	mu             sync.Mutex
	changesSummary string
	filesChanged   map[string]struct{}
	turns          int
	usage          result.TokenUsage
	isError        bool
	errorSubtype   string
	// model is the most recently observed Claude model identifier
	// (e.g., "claude-opus-4-7"). Claude Code emits it on the
	// system.init event and also on each assistant message; last seen
	// wins, so a mid-run reroute (rare but possible) is reflected.
	model string
	// prTitle is the agent-produced short title parsed out of the
	// final assistant message's <pr_title>...</pr_title> trailer (see
	// finalMessageInstruction in driver.go).
	prTitle string
}

func newStreamParser() *streamParser {
	return &streamParser{filesChanged: make(map[string]struct{})}
}

// Consume reads stream-json lines from r and updates internal state.
// Returns when r is exhausted.
func (p *streamParser) Consume(r io.Reader) {
	scanner := bufio.NewScanner(r)
	// Raise the 64 KiB default: tool_use inputs and result summaries can be large.
	const maxTokenSize = 10 * 1024 * 1024
	scanner.Buffer(make([]byte, 0, 64*1024), maxTokenSize)

	for scanner.Scan() {
		p.processLine(scanner.Bytes())
	}
}

// State returns the accumulated parse result as an agent.ParsedState.
// Safe to call concurrently with Consume — locks while copying out
// the snapshot so the caller never observes a torn read.
func (p *streamParser) State() agent.ParsedState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return agent.ParsedState{
		ChangesSummary: p.changesSummary,
		FilesChanged:   p.filesChangedSortedLocked(),
		TokenUsage:     p.usage,
		Turns:          p.turns,
		IsError:        p.isError,
		IsAuthFailure:  p.isAuthFailureLocked(),
		Model:          p.model,
		PRTitle:        p.prTitle,
	}
}

func (p *streamParser) processLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}

	var event streamEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return
	}

	switch event.Type {
	case "system":
		// Claude Code emits {"type":"system","subtype":"init","model":"..."}
		// as the first line of every run. Captured here so the model
		// is available even on runs that fail before any assistant
		// message arrives.
		if event.Subtype == "init" && event.Model != "" {
			p.mu.Lock()
			p.model = event.Model
			p.mu.Unlock()
		}
	case "assistant":
		p.processAssistantMessage(event.Message)
	case "result":
		p.processResultEvent(event)
	}
}

type streamEvent struct {
	Type     string          `json:"type"`
	Message  json.RawMessage `json:"message"`
	Result   string          `json:"result"`
	Usage    *streamUsage    `json:"usage"`
	NumTurns int             `json:"num_turns"`
	IsError  bool            `json:"is_error"`
	Subtype  string          `json:"subtype"`
	Model    string          `json:"model"`
}

type streamUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

type assistantMessage struct {
	Content []contentBlock `json:"content"`
	// Model is the model handling this specific message. Tracked as a
	// fallback for runs where system.init didn't fire (e.g., truncated
	// stream) — last seen wins, mirroring Claude Code's own rerouting.
	Model string `json:"model"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type fileInput struct {
	FilePath string `json:"file_path"`
}

func (p *streamParser) processAssistantMessage(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var msg assistantMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	// Lock only around the actual field mutation; JSON parse above is
	// read-only on its inputs and shouldn't hold the lock.
	p.mu.Lock()
	defer p.mu.Unlock()
	if msg.Model != "" {
		p.model = msg.Model
	}
	for _, b := range msg.Content {
		if b.Type != "tool_use" || !isFileModifyingTool(b.Name) {
			continue
		}
		var input fileInput
		if err := json.Unmarshal(b.Input, &input); err != nil {
			continue
		}
		if input.FilePath != "" {
			p.filesChanged[input.FilePath] = struct{}{}
		}
	}
}

func (p *streamParser) processResultEvent(event streamEvent) {
	summary, prTitle := splitPRTitleTrailer(event.Result)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changesSummary = summary
	p.prTitle = prTitle
	p.turns = event.NumTurns
	p.isError = event.IsError
	p.errorSubtype = event.Subtype
	if event.Usage != nil {
		p.usage = result.TokenUsage{
			InputTokens:     event.Usage.InputTokens,
			OutputTokens:    event.Usage.OutputTokens,
			CacheReadTokens: event.Usage.CacheReadInputTokens,
		}
	}
}

// splitPRTitleTrailer extracts the <pr_title>...</pr_title> block from
// the agent's final result text. Returns the (summary-without-the-tag,
// pr_title) pair. The tag is removed from the summary regardless of
// where it appeared, and the summary is trimmed of trailing whitespace
// left behind by the removal. When no tag is present (agent ignored
// the instruction), returns (input, "").
func splitPRTitleTrailer(result string) (summary, prTitle string) {
	match := prTitleRe.FindStringSubmatchIndex(result)
	if match == nil {
		return result, ""
	}
	prTitle = strings.TrimSpace(result[match[2]:match[3]])
	summary = strings.TrimRightFunc(result[:match[0]]+result[match[1]:], isSpaceOrNewline)
	return summary, prTitle
}

func isSpaceOrNewline(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func isFileModifyingTool(name string) bool {
	switch name {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

// filesChangedSortedLocked returns a sorted snapshot of the changed-file
// set. Caller must hold p.mu.
func (p *streamParser) filesChangedSortedLocked() []string {
	out := make([]string, 0, len(p.filesChanged))
	for f := range p.filesChanged {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// isAuthFailureLocked reports whether the parsed state looks like an
// auth or rate-limit error rather than a generic execution failure.
// Caller must hold p.mu.
func (p *streamParser) isAuthFailureLocked() bool {
	if !p.isError {
		return false
	}
	subtype := strings.ToLower(p.errorSubtype)
	if strings.Contains(subtype, "auth") || strings.Contains(subtype, "api_key") {
		return true
	}
	return agent.HasAuthKeyword(strings.ToLower(p.changesSummary))
}

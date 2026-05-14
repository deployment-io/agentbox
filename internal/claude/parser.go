package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/result"
)

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
}

type streamUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

type assistantMessage struct {
	Content []contentBlock `json:"content"`
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
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changesSummary = event.Result
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

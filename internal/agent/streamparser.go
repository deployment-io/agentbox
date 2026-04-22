package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/deployment-io/agentbox/internal/result"
)

// streamParser consumes Claude Code's --output-format=stream-json events
// line-by-line and accumulates structured state that becomes part of
// the Outcome.
//
// The parser is forgiving by design: a malformed line (invalid JSON,
// unexpected field type, unknown event type) is silently ignored. A
// broken line in the middle of a stream doesn't prevent us from
// capturing whatever valid state came before or after it.
type streamParser struct {
	changesSummary string
	filesChanged   map[string]struct{}
	turns          int
	usage          result.TokenUsage
	isError        bool
	errorSubtype   string
}

func newStreamParser() *streamParser {
	return &streamParser{
		filesChanged: make(map[string]struct{}),
	}
}

// Consume reads stream-json lines from r and updates internal state.
// Returns when r is exhausted. Safe to call from a goroutine.
func (p *streamParser) Consume(r io.Reader) {
	scanner := bufio.NewScanner(r)
	// Claude Code can emit large events (big tool_use inputs or result
	// messages with full changes_summary). Raise the default 64 KiB limit
	// to 10 MiB so we don't silently truncate.
	const maxTokenSize = 10 * 1024 * 1024
	scanner.Buffer(make([]byte, 0, 64*1024), maxTokenSize)

	for scanner.Scan() {
		p.processLine(scanner.Bytes())
	}
	// Don't propagate scanner.Err(): parse errors are non-fatal, and
	// the outcome carries whatever state we captured.
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

// streamEvent is a superset of the fields agentbox cares about across
// all stream-json event types. Unused fields stay nil.
type streamEvent struct {
	Type     string          `json:"type"`
	Message  json.RawMessage `json:"message"`
	Result   string          `json:"result"`
	Usage    *streamUsage    `json:"usage"`
	NumTurns int             `json:"num_turns"`
	IsError  bool            `json:"is_error"`
	Subtype  string          `json:"subtype"`
}

// streamUsage is Claude Code's token-usage shape in the final result event.
type streamUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

// assistantMessage captures just the content-blocks structure — we dig
// inside looking for tool_use blocks that modified files.
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

// isFileModifyingTool returns true for tools that modify files on disk.
func isFileModifyingTool(name string) bool {
	switch name {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

// filesChangedSorted returns the sorted slice of changed file paths.
func (p *streamParser) filesChangedSorted() []string {
	out := make([]string, 0, len(p.filesChanged))
	for f := range p.filesChanged {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// isAuthFailure reports whether the parsed state indicates an auth or
// rate-limit error (distinguishing it from a generic execution failure).
// Consumers use this to return exit code 2 instead of 1.
func (p *streamParser) isAuthFailure() bool {
	if !p.isError {
		return false
	}
	subtype := strings.ToLower(p.errorSubtype)
	if strings.Contains(subtype, "auth") || strings.Contains(subtype, "api_key") {
		return true
	}
	return hasAuthKeyword(strings.ToLower(p.changesSummary))
}

// hasAuthKeyword returns true if s contains any keyword that typically
// indicates authentication or rate-limiting trouble. Heuristic — errors
// on the side of false-positives (OK: we surface a more actionable
// message; worst case the user updates a correct key).
func hasAuthKeyword(s string) bool {
	keywords := []string{
		"api key",
		"api_key",
		"apikey",
		"unauthorized",
		"authentication",
		"auth failed",
		"invalid key",
		"rate limit",
		"rate_limit",
		"ratelimit",
		"quota",
		"throttl",
		"401",
		"429",
		"access denied",
		"accessdenied",
		"not enabled in region",
		"model access",
	}
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

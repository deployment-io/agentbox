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
// line-by-line. Malformed or unknown events are silently skipped so one
// bad line doesn't drop the rest of the stream.
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
	// Raise the 64 KiB default: tool_use inputs and result summaries can be large.
	const maxTokenSize = 10 * 1024 * 1024
	scanner.Buffer(make([]byte, 0, 64*1024), maxTokenSize)

	for scanner.Scan() {
		p.processLine(scanner.Bytes())
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

func isFileModifyingTool(name string) bool {
	switch name {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

func (p *streamParser) filesChangedSorted() []string {
	out := make([]string, 0, len(p.filesChanged))
	for f := range p.filesChanged {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// isAuthFailure reports whether the parsed state looks like an auth or
// rate-limit error rather than a generic execution failure.
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

// hasAuthKeyword is a heuristic scan for auth / rate-limit / model-access
// trouble. Errs toward false-positives — a misclassified auth message is
// still actionable; missing one leaves the user without guidance.
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

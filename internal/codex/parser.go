package codex

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

// jsonlParser consumes Codex CLI's `codex exec --json` event stream
// (newline-delimited JSON) and accumulates agent.ParsedState. Malformed or
// unknown lines/events are skipped so one bad line doesn't drop the rest of
// the stream — the same resilience contract as the claude parser.
//
// State() is safe to call concurrently with Consume(): mu guards every
// field. Unlike the claude parser (which sets turns/usage only on its final
// result event), this parser updates them incrementally on each
// turn.completed — which is exactly what lets agentbox enforce MAX_TURNS /
// TOKEN_BUDGET for Codex from the live stream (Codex's CLI has no such
// flags). See agent.Run's limit watcher.
type jsonlParser struct {
	mu           sync.Mutex
	finalMessage string // latest agent_message text; the final one carries the summary + trailers
	filesChanged map[string]struct{}
	turns        int
	usage        result.TokenUsage
	isError      bool
	errorMessage string
	model        string
}

func newJSONLParser() *jsonlParser {
	return &jsonlParser{filesChanged: make(map[string]struct{})}
}

// Consume reads JSONL events from r and updates internal state. Returns
// when r is exhausted.
func (p *jsonlParser) Consume(r io.Reader) {
	scanner := bufio.NewScanner(r)
	// Raise the 64 KiB default: agent messages and tool outputs can be large.
	const maxTokenSize = 10 * 1024 * 1024
	scanner.Buffer(make([]byte, 0, 64*1024), maxTokenSize)
	for scanner.Scan() {
		p.processLine(scanner.Bytes())
	}
}

// State returns the accumulated parse result. Safe to call concurrently
// with Consume. The PR-title and verify trailers are split out of the
// latest agent message on each call (cheap; the message is small).
func (p *jsonlParser) State() agent.ParsedState {
	p.mu.Lock()
	defer p.mu.Unlock()
	summary, prTitle := splitPRTitleTrailer(p.finalMessage)
	summary, verify := splitVerifyTrailer(summary)
	return agent.ParsedState{
		ChangesSummary: summary,
		FilesChanged:   p.filesChangedSortedLocked(),
		TokenUsage:     p.usage,
		Turns:          p.turns,
		IsError:        p.isError,
		IsAuthFailure:  p.isAuthFailureLocked(),
		FailureReason:  p.failureReasonLocked(),
		Model:          p.model,
		PRTitle:        prTitle,
		VerifyResult:   verify,
	}
}

func (p *jsonlParser) processLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var ev codexEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "thread.started":
		if ev.Model != "" {
			p.mu.Lock()
			p.model = ev.Model
			p.mu.Unlock()
		}
	case "turn.completed":
		p.mu.Lock()
		p.turns++
		if ev.Usage != nil {
			// Codex reports per-turn usage; accumulate the billed totals
			// across turns. Reasoning tokens are output-side.
			p.usage.InputTokens += ev.Usage.InputTokens
			p.usage.OutputTokens += ev.Usage.OutputTokens + ev.Usage.ReasoningOutputTokens
			p.usage.CacheReadTokens += ev.Usage.CachedInputTokens
		}
		p.mu.Unlock()
	case "turn.failed", "error":
		p.setError(ev.failureMessage())
	case "item.completed":
		p.processItem(ev.Item)
	}
}

type codexEvent struct {
	Type    string          `json:"type"`
	Model   string          `json:"model"`
	Usage   *codexUsage     `json:"usage"`
	Item    json.RawMessage `json:"item"`
	Error   *codexError     `json:"error"`
	Message string          `json:"message"`
}

type codexUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

type codexError struct {
	Message string `json:"message"`
}

// failureMessage extracts a human-readable error from a turn.failed /
// error event, tolerating both the nested {"error":{"message":...}} and
// the flat {"message":...} shapes (the exact error schema isn't documented).
func (e codexEvent) failureMessage() string {
	if e.Error != nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return e.Message
}

// codexItem is the nested object on item.started / item.completed. The
// discriminator is "type" (e.g. "agent_message", "command_execution",
// "file_change"). agent_message carries the agent's text; file_change
// carries the edited paths. The file_change shape is best-effort — it is
// not formally documented, and files_changed in result.json is display-only
// (the runner commits from the actual git diff), so an imperfect match just
// yields an empty list rather than a broken run.
type codexItem struct {
	Type    string        `json:"type"`
	Text    string        `json:"text"`
	Changes []codexChange `json:"changes"`
}

type codexChange struct {
	Path string `json:"path"`
}

func (p *jsonlParser) processItem(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var it codexItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch it.Type {
	case "agent_message":
		if strings.TrimSpace(it.Text) != "" {
			// Latest agent message wins; the final one carries the changes
			// summary plus the <verify> / <pr_title> trailers.
			p.finalMessage = it.Text
		}
	case "file_change":
		for _, ch := range it.Changes {
			if ch.Path != "" {
				p.filesChanged[ch.Path] = struct{}{}
			}
		}
	}
}

func (p *jsonlParser) setError(msg string) {
	p.mu.Lock()
	p.isError = true
	if msg != "" && p.errorMessage == "" {
		p.errorMessage = msg
	}
	p.mu.Unlock()
}

// filesChangedSortedLocked returns a sorted snapshot of the changed-file
// set. Caller must hold p.mu.
func (p *jsonlParser) filesChangedSortedLocked() []string {
	out := make([]string, 0, len(p.filesChanged))
	for f := range p.filesChanged {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// failureReasonLocked surfaces the agent's own error text (prefixed with
// the binary name by the orchestrator). Codex carries no structured
// subtype like Claude's error_max_turns, so the raw message is the best
// signal. Caller must hold p.mu.
func (p *jsonlParser) failureReasonLocked() string {
	if p.isError {
		return p.errorMessage
	}
	return ""
}

// isAuthFailureLocked reports whether the failure looks auth / rate-limit
// related, so the orchestrator can exit 2 (ExitAuthFailure). Caller must
// hold p.mu.
func (p *jsonlParser) isAuthFailureLocked() bool {
	if !p.isError {
		return false
	}
	return agent.HasAuthKeyword(strings.ToLower(p.errorMessage))
}

// --- Final-message trailer parsing ---------------------------------------
//
// These mirror the helpers in internal/claude/parser.go. Both agents are
// instructed (via finalMessageInstruction) to end their final message with
// a <verify>{json}</verify> block and a <pr_title>...</pr_title> trailer, so
// the runner's handling stays agent-agnostic. Kept local rather than shared
// in internal/agent/ until a third agent justifies the extraction (Rule of
// Three).

var prTitleRe = regexp.MustCompile(`(?s)<pr_title>(.*?)</pr_title>`)

var verifyRe = regexp.MustCompile(`(?s)<verify>(.*?)</verify>`)

// splitPRTitleTrailer extracts the <pr_title>...</pr_title> block, returning
// (summary-without-the-tag, pr_title). When absent, returns (input, "").
func splitPRTitleTrailer(text string) (summary, prTitle string) {
	match := prTitleRe.FindStringSubmatchIndex(text)
	if match == nil {
		return text, ""
	}
	prTitle = strings.TrimSpace(text[match[2]:match[3]])
	summary = strings.TrimRightFunc(text[:match[0]]+text[match[1]:], isSpaceOrNewline)
	return summary, prTitle
}

// splitVerifyTrailer extracts and parses the <verify>{json}</verify> block.
// Returns the summary with the tag removed and the parsed VerifyResult — nil
// when the block is absent or the JSON doesn't parse. A missing/garbled
// block is "no verify reported", never a hard error; the tag is still
// stripped so it can't leak into the PR body.
func splitVerifyTrailer(text string) (summary string, verify *result.VerifyResult) {
	match := verifyRe.FindStringSubmatchIndex(text)
	if match == nil {
		return text, nil
	}
	payload := strings.TrimSpace(text[match[2]:match[3]])
	summary = strings.TrimRightFunc(text[:match[0]]+text[match[1]:], isSpaceOrNewline)
	var vr result.VerifyResult
	if err := json.Unmarshal([]byte(payload), &vr); err != nil {
		return summary, nil
	}
	return summary, &vr
}

func isSpaceOrNewline(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

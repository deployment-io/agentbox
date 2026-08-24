package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	// costUSD is Claude Code's self-reported total_cost_usd from the final
	// result event (USD). Pointer so "unreported" stays distinct from $0.
	costUSD      *float64
	isError      bool
	errorSubtype string
	// model is the most recently observed Claude model identifier
	// (e.g., "claude-opus-4-7"). Claude Code emits it on the
	// system.init event and also on each assistant message; last seen
	// wins, so a mid-run reroute (rare but possible) is reflected.
	model string
	// prTitle is the agent-produced short title parsed out of the
	// final assistant message's <pr_title>...</pr_title> trailer (see
	// finalMessageInstruction in driver.go).
	prTitle string
	// verifyResult is the agent's self-reported build/test outcome, parsed
	// from the final message's <verify>{json}</verify> trailer. Nil when the
	// agent emitted none (or the JSON didn't parse).
	verifyResult *result.VerifyResult
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
		FailureReason:  p.failureReasonLocked(),
		ErrorSubtype:   p.errorSubtypeLocked(),
		Model:          p.model,
		CostUSD:        p.costUSD,
		PRTitle:        p.prTitle,
		VerifyResult:   p.verifyResult,
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
	// TotalCostUSD is Claude Code's computed total run cost in USD on the
	// final result event. Pointer to distinguish absent from a real 0.
	TotalCostUSD *float64 `json:"total_cost_usd"`
}

type streamUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
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
	summary, verify := splitVerifyTrailer(summary)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changesSummary = summary
	p.prTitle = prTitle
	p.verifyResult = verify
	p.turns = event.NumTurns
	p.isError = event.IsError
	p.errorSubtype = event.Subtype
	if event.Usage != nil {
		p.usage = result.TokenUsage{
			InputTokens:         event.Usage.InputTokens,
			OutputTokens:        event.Usage.OutputTokens,
			CacheReadTokens:     event.Usage.CacheReadInputTokens,
			CacheCreationTokens: event.Usage.CacheCreationInputTokens,
		}
	}
	if event.TotalCostUSD != nil {
		p.costUSD = event.TotalCostUSD
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

// verifyRe matches the <verify>{json}</verify> trailer carrying the agent's
// self-reported build/test outcome (see finalMessageInstruction).
var verifyRe = regexp.MustCompile(`(?s)<verify>(.*?)</verify>`)

// splitVerifyTrailer extracts and parses the <verify> JSON block from the
// agent's final message. Returns the summary with the tag removed and the
// parsed VerifyResult — nil when the block is absent or the JSON doesn't
// parse. A missing or garbled block is "no verify reported", never a hard
// error; the tag is still stripped so it can't leak into the PR body.
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

// failureReasonLocked maps Claude Code's result-event subtype to a
// human-readable failure predicate (e.g. "reached its turn limit after
// 26 turns ..."), meant to be prefixed with the agent binary name by
// the caller. Returns "" when the subtype carries no actionable detail,
// letting the orchestrator fall back to the raw exit error + stderr.
//
// Only error_max_turns is special-cased: it's the one failure whose
// remedy (raise max_turns) the message can name, and Claude Code can
// signal it while exiting 0, so a status-only message would be
// actively misleading. error_during_execution and friends are left to
// the fallback path, which surfaces the agent's stderr — more useful
// than a generic restatement of the subtype. Caller must hold p.mu.
func (p *streamParser) failureReasonLocked() string {
	if !p.isError {
		return ""
	}
	// A Bedrock model-access denial is the other failure whose remedy the
	// message can name — and the one where Claude Code's own words send the
	// reader in TWO wrong directions at once, so it earns the same treatment
	// as max_turns. See bedrockAccessReason.
	if reason := bedrockAccessReason(p.changesSummary); reason != "" {
		return reason
	}
	if p.errorSubtype != "error_max_turns" {
		return ""
	}
	if p.turns > 0 {
		return fmt.Sprintf("reached its turn limit after %d turns; raise max_turns to allow more steps", p.turns)
	}
	return "reached its turn limit; raise max_turns to allow more steps"
}

// bedrockAccessReason turns a Bedrock model-access denial into an instruction,
// or returns "" when the failure is anything else.
//
// The mirror of opencode's bedrockAccessHint, needed here because the default
// agent's version of this error misleads twice over. A live run produced:
//
//	Failed to authenticate. API Error: 403 anthropic.claude-opus-5 is not
//	available for this account. ... contact AWS Sales ...
//
// Authentication had SUCCEEDED — the runner assumed the Bedrock role and
// vended working credentials; only the per-model access grant was missing. And
// "contact AWS Sales" is the remedy for provisioned-throughput arrangements,
// not for the model-access console page that actually fixes this. Someone
// following the message as written checks their credentials, then emails AWS.
//
// Same three gates as the opencode hint, for the same reasons — each removes a
// way of pointing someone at the wrong console page:
//
//	Bedrock mode IS on    — the advice is Bedrock-specific. Anthropic Direct
//	                        answering 403 with similar wording must not send
//	                        anyone to the AWS console.
//	"403" IS in the text  — the sentence alone could be an echo inside an
//	                        unrelated failure. Claude Code renders the status
//	                        into prose ("API Error: 403 ..."), so unlike
//	                        opencode there is no structured field to read;
//	                        if that rendering ever changes, the hint silently
//	                        stops firing, which degrades to today's behaviour.
//	the SENTENCE matches  — 403 also covers denied IAM actions and bad
//	                        signatures, which need a different fix entirely.
//
// The misleading "Failed to authenticate." prefix is stripped rather than
// corrected in place: the words after it are AWS's own and worth keeping, but
// leading with a false diagnosis buries the real one.
func bedrockAccessReason(summary string) string {
	if os.Getenv("CLAUDE_CODE_USE_BEDROCK") != "1" {
		return ""
	}
	if !strings.Contains(summary, "403") || !strings.Contains(summary, "is not available for this account") {
		return ""
	}
	detail := strings.TrimSpace(strings.TrimPrefix(summary, "Failed to authenticate."))
	region := os.Getenv("AWS_REGION")
	where := "this region"
	if region != "" {
		where = region
	}
	return "cannot use this model on Bedrock: " + detail +
		" — enable model access for it in the Bedrock console in " + where +
		", or configure a direct provider for this model."
}

// errorSubtypeLocked returns the raw result-event subtype when the run
// failed (e.g. "error_during_execution"), else "". Lets the
// orchestrator name the failure class as a last resort so the subtype
// is never lost from the surfaced error. Caller must hold p.mu.
func (p *streamParser) errorSubtypeLocked() string {
	if !p.isError {
		return ""
	}
	return p.errorSubtype
}

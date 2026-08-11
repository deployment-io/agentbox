package opencode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/deployment-io/agentbox/internal/agent"
	"github.com/deployment-io/agentbox/internal/result"
)

// jsonlParser consumes opencode's `opencode run --format json` event stream
// (newline-delimited JSON) and accumulates agent.ParsedState. Malformed or
// unknown lines/events are skipped so one bad line doesn't drop the rest of the
// stream — the same resilience contract as the claude/codex parsers.
//
// State() is safe to call concurrently with Consume(): mu guards every field.
// Like the codex parser (and unlike claude), turns + usage update incrementally
// — once per step-finish event — which is exactly what lets agentbox enforce
// MAX_TURNS / TOKEN_BUDGET for opencode from the live stream (opencode's CLI has
// no such flags). See agent.Run's limit watcher. opencode DOES report cost
// (priced off models.dev), so CostUSD is populated, unlike codex.
//
// Schema verified against a captured `opencode run --format json` (opencode
// 1.17.9, 2026-07-19): the top-level discriminators are "step_finish" /
// "tool_use" / "text" / "error"; token usage lives at
// part.tokens.{input,output,cache.{read,write}} and cost at part.cost; text
// arrives as one full part per message (not incremental deltas). A future schema
// mismatch degrades to "fewer fields populated", never a crash.
type jsonlParser struct {
	mu           sync.Mutex
	finalMessage string // latest assistant text; the final one carries the summary + trailers
	filesChanged map[string]struct{}
	turns        int
	usage        result.TokenUsage
	cost         float64
	costReported bool
	isError      bool
	errorMessage string
	model        string
}

func newJSONLParser() *jsonlParser {
	return &jsonlParser{filesChanged: make(map[string]struct{})}
}

// Consume reads JSONL events from r and updates internal state. Returns when r
// is exhausted.
func (p *jsonlParser) Consume(r io.Reader) {
	scanner := bufio.NewScanner(r)
	// Raise the 64 KiB default: agent messages and tool outputs can be large.
	const maxTokenSize = 10 * 1024 * 1024
	scanner.Buffer(make([]byte, 0, 64*1024), maxTokenSize)
	for scanner.Scan() {
		p.processLine(scanner.Bytes())
	}
}

// State returns the accumulated parse result. Safe to call concurrently with
// Consume. The PR-title and verify trailers are split out of the latest agent
// message on each call (cheap; the message is small).
func (p *jsonlParser) State() agent.ParsedState {
	p.mu.Lock()
	defer p.mu.Unlock()
	summary, prTitle := splitPRTitleTrailer(p.finalMessage)
	summary, verify := splitVerifyTrailer(summary)
	var costPtr *float64
	if p.costReported {
		c := p.cost
		costPtr = &c
	}
	return agent.ParsedState{
		ChangesSummary: summary,
		FilesChanged:   p.filesChangedSortedLocked(),
		TokenUsage:     p.usage,
		Turns:          p.turns,
		IsError:        p.isError,
		IsAuthFailure:  p.isAuthFailureLocked(),
		FailureReason:  p.failureReasonLocked(),
		Model:          p.model,
		CostUSD:        costPtr,
		PRTitle:        prTitle,
		VerifyResult:   verify,
	}
}

func (p *jsonlParser) processLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var ev opencodeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "text":
		p.handleText(ev.Part)
	case "step_finish":
		p.handleStepFinish(ev.Part)
	case "tool_use":
		p.handleTool(ev.Part)
	case "error":
		p.setError(ev.errorMessage())
	}
}

// opencodeEvent is the top-level JSON object of one stream line. Part carries
// the event-specific payload (text, step usage, tool call); Error is set on
// error events.
type opencodeEvent struct {
	Type string          `json:"type"`
	Part json.RawMessage `json:"part"`
	// Error is kept RAW rather than decoded into a struct.
	//
	// Decoding it cost three releases of guesswork. The struct read `message`
	// and `name`, so an event carrying its detail anywhere else lost it —
	// which is exactly what `APIError` does: the name alone says nothing about
	// what failed, and a live Bedrock run produced precisely that, twice, with
	// no way to tell whether it was model access, credentials or routing.
	//
	// Cherry-picking fields cannot be made right by adding more fields, because
	// the shape is opencode's to change. Keeping the bytes means whatever it
	// sends survives to a human.
	Error json.RawMessage `json:"error"`
}

type opencodeError struct {
	Message string `json:"message"`
	Name    string `json:"name"`
	// Data is where the provider's own words actually land. opencode wraps an
	// AI-SDK error, and a live Bedrock 403 arrived with the top-level message
	// EMPTY and the sentence nested here — which is why reading only the two
	// fields above reported a bare "APIError" three times running.
	Data opencodeErrorData `json:"data"`
}

type opencodeErrorData struct {
	Message    string `json:"message"`
	StatusCode int    `json:"statusCode"`
}

// bedrockAccessHint turns "this model is not available for this account" into
// an instruction, or returns "" when the error is something else.
//
// The raw sentence names the problem and not the fix. AWS suggests contacting
// sales; the actual remedy is usually two clicks in the Bedrock console, and
// failing that a direct provider — neither of which the message mentions.
//
// Matched on the SENTENCE, not on the status code alone: 403 covers plenty of
// unrelated failures (a bad signature, a denied IAM action) and rewriting those
// as "enable model access" would send someone to the wrong page — the same
// mistake as conflating missing credentials with missing model access.
func bedrockAccessHint(detail string, statusCode int) string {
	if statusCode != 403 || !strings.Contains(detail, "is not available for this account") {
		return ""
	}
	region := os.Getenv("AWS_REGION")
	where := "this region"
	if region != "" {
		where = region
	}
	return detail +
		" — enable model access for it in the Bedrock console in " + where +
		", or configure a direct provider for this model."
}

// opencodePart is the union of the part shapes we read across event types. JSON
// fields absent for a given event simply stay zero.
type opencodePart struct {
	Text    string             `json:"text"`
	Tokens  *opencodeTokens    `json:"tokens"`
	Cost    *float64           `json:"cost"`
	ModelID string             `json:"modelID"`
	Tool    string             `json:"tool"`
	State   *opencodeToolState `json:"state"`
}

type opencodeTokens struct {
	Input     int           `json:"input"`
	Output    int           `json:"output"`
	Reasoning int           `json:"reasoning"`
	Cache     opencodeCache `json:"cache"`
}

type opencodeCache struct {
	Read  int `json:"read"`
	Write int `json:"write"`
}

// opencodeToolState carries a tool call's input args. write/edit put the target
// in state.input.filePath; apply_patch carries no filePath — its target paths are
// embedded in state.input.patchText and parsed by applyPatchPaths.
type opencodeToolState struct {
	Input struct {
		FilePath  string `json:"filePath"`
		PatchText string `json:"patchText"`
	} `json:"input"`
}

// errorMessage renders an error event's detail, or "" when it carries none.
//
// PREFERS A HUMAN SENTENCE, FALLS BACK TO THE RAW PAYLOAD. name+message when
// both are present, message alone when that is all there is — and when the
// event carries a name but no message, the whole JSON object, because a bare
// name is not a diagnosis. "APIError" told us nothing across two live Bedrock
// failures while the useful part sat unread in fields this never decoded.
//
// Returning "" for a genuinely empty error is deliberate. A generic reason is
// WORSE than none: failureMessage prefers FailureReason over every other
// signal, so a placeholder pre-empts the stderr tail that might have explained
// the failure — and, being self-describing, produced "opencode opencode
// reported an error". Empty lets the fallbacks run.
func (e opencodeEvent) errorMessage() string {
	if len(e.Error) == 0 {
		return ""
	}
	var oe opencodeError
	_ = json.Unmarshal(e.Error, &oe) // a non-object error still has its raw form

	// The real sentence is usually NESTED, under data.message — which is why
	// reading only the top-level {message, name} produced a bare "APIError"
	// across three live Bedrock failures. The provider's own words are there.
	detail := oe.Message
	if detail == "" {
		detail = strings.TrimSpace(strings.TrimPrefix(oe.Data.Message, "undefined:"))
	}
	// A provider saying "you cannot use this model" is worth turning into an
	// instruction, since the raw sentence names the problem but not the fix.
	if hint := bedrockAccessHint(detail, oe.Data.StatusCode); hint != "" {
		return hint
	}
	switch {
	case oe.Name != "" && detail != "":
		return oe.Name + ": " + detail
	case detail != "":
		return detail
	}
	// No message. Fall back to the raw object ONLY when it holds more than the
	// two fields already read — otherwise `{"name":"X"}` is just a noisier way
	// of writing X. The point is to surface detail we would otherwise drop, not
	// to print JSON for its own sake.
	if hasFieldsBeyond(e.Error, "name", "message") {
		if raw := compactJSON(e.Error); raw != "" {
			return raw
		}
	}
	return oe.Name
}

// hasFieldsBeyond reports whether a JSON object carries keys other than the
// named ones. A non-object (string, array) counts as extra: it holds something
// the struct decode could not have reached.
func hasFieldsBeyond(raw json.RawMessage, known ...string) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return len(bytes.TrimSpace(raw)) > 0
	}
	for k, v := range obj {
		if len(bytes.TrimSpace(v)) == 0 || string(bytes.TrimSpace(v)) == "null" {
			continue
		}
		if !slices.Contains(known, k) {
			return true
		}
	}
	return false
}

// compactJSON renders raw JSON on one line, capped, for embedding in an error
// string. Capped because this lands in a Job document and a provider can echo a
// whole request body back; the head is where the useful fields are.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	const maxRunes = 400
	out := []rune(buf.String())
	if len(out) > maxRunes {
		return string(out[:maxRunes]) + "…"
	}
	return string(out)
}

func (p *jsonlParser) handleText(raw json.RawMessage) {
	var part opencodePart
	if err := json.Unmarshal(raw, &part); err != nil {
		return
	}
	if strings.TrimSpace(part.Text) == "" {
		return
	}
	p.mu.Lock()
	// Latest assistant text wins; the final one carries the changes summary plus
	// the <verify> / <pr_title> trailers. Confirmed against a real run: opencode
	// emits one full text part per message (not incremental deltas), so the final
	// message's text is complete.
	p.finalMessage = part.Text
	if part.ModelID != "" {
		p.model = part.ModelID
	}
	p.mu.Unlock()
}

func (p *jsonlParser) handleStepFinish(raw json.RawMessage) {
	var part opencodePart
	_ = json.Unmarshal(raw, &part) // tolerate a missing/changed part shape
	p.mu.Lock()
	p.turns++
	if part.Tokens != nil {
		// Anthropic-style accounting (opencode's default shape): input/output are
		// the billed counts and cache read/write are separate buckets, so they're
		// recorded distinctly (the limit watcher sums input+output, not cache).
		p.usage.InputTokens += part.Tokens.Input
		p.usage.OutputTokens += part.Tokens.Output
		p.usage.CacheReadTokens += part.Tokens.Cache.Read
		p.usage.CacheCreationTokens += part.Tokens.Cache.Write
	}
	if part.Cost != nil {
		p.cost += *part.Cost
		p.costReported = true
	}
	if part.ModelID != "" {
		p.model = part.ModelID
	}
	p.mu.Unlock()
}

// handleTool records files a MUTATING tool changed. Only write/edit/apply_patch
// touch the working tree; read/grep/glob/bash/etc. are ignored so files_changed
// reflects real edits, not every file the agent inspected — an unbounded set on a
// large repo. files_changed is display-only (the runner commits from the git
// diff), so under-reporting is safe, whereas over-reporting bloats result.json
// and the Job.Output it flows into. Hence the mutating-tool allowlist. Confirmed
// tool names + path fields against opencode source (write/edit → filePath;
// apply_patch → paths inside patchText).
func (p *jsonlParser) handleTool(raw json.RawMessage) {
	var part opencodePart
	if err := json.Unmarshal(raw, &part); err != nil || part.State == nil {
		return
	}
	var paths []string
	switch part.Tool {
	case "write", "edit":
		if fp := part.State.Input.FilePath; fp != "" {
			paths = []string{fp}
		}
	case "apply_patch":
		paths = applyPatchPaths(part.State.Input.PatchText)
	default:
		return // non-mutating (read/grep/glob/bash/webfetch/…) — don't track
	}
	if len(paths) == 0 {
		return
	}
	p.mu.Lock()
	for _, fp := range paths {
		p.filesChanged[fp] = struct{}{}
	}
	p.mu.Unlock()
}

// applyPatchFileRe matches the per-file headers in an apply_patch patchText body:
// "*** Add File: <path>", "*** Update File: <path>", "*** Delete File: <path>".
var applyPatchFileRe = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$`)

// applyPatchPaths extracts target paths from an apply_patch patchText. That tool
// has no filePath arg. Best-effort: if the patch format shifts we under-report,
// which handleTool treats as safe. Returns nil when nothing matches.
func applyPatchPaths(patchText string) []string {
	if patchText == "" {
		return nil
	}
	var out []string
	for _, m := range applyPatchFileRe.FindAllStringSubmatch(patchText, -1) {
		if pth := strings.TrimSpace(m[1]); pth != "" {
			out = append(out, pth)
		}
	}
	return out
}

func (p *jsonlParser) setError(msg string) {
	p.mu.Lock()
	p.isError = true
	if msg != "" && p.errorMessage == "" {
		p.errorMessage = msg
	}
	p.mu.Unlock()
}

// filesChangedSortedLocked returns a sorted snapshot of the changed-file set.
// Caller must hold p.mu.
func (p *jsonlParser) filesChangedSortedLocked() []string {
	out := make([]string, 0, len(p.filesChanged))
	for f := range p.filesChanged {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// failureReasonLocked surfaces opencode's own error text (prefixed with the
// binary name by the orchestrator). Caller must hold p.mu.
func (p *jsonlParser) failureReasonLocked() string {
	if p.isError {
		return p.errorMessage
	}
	return ""
}

// isAuthFailureLocked reports whether the failure looks auth / rate-limit
// related, so the orchestrator can exit 2 (ExitAuthFailure). Caller must hold
// p.mu.
func (p *jsonlParser) isAuthFailureLocked() bool {
	if !p.isError {
		return false
	}
	return agent.HasAuthKeyword(strings.ToLower(p.errorMessage))
}

// --- Final-message trailer parsing ---------------------------------------
//
// These mirror the helpers in internal/codex/parser.go (themselves mirrored
// from internal/claude). All three agents are instructed (via
// finalMessageInstruction) to end their final message with a
// <verify>{json}</verify> block and a <pr_title>...</pr_title> trailer, so the
// runner's handling stays agent-agnostic. opencode is the third copy — see the
// Rule-of-Three extraction note in driver.go / PLAN_tasks_opencode_support.md.

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
// when the block is absent or the JSON doesn't parse. A missing/garbled block is
// "no verify reported", never a hard error; the tag is still stripped so it
// can't leak into the PR body.
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

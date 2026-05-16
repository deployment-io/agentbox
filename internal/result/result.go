// Package result defines the /result.json schema and writing semantics.
//
// See docs/CONTRACT.md for the full output spec.
package result

import (
	"encoding/json"
	"os"
	"unicode/utf8"
)

// schemaVersion is bumped on breaking changes to the result.json shape.
const schemaVersion = 1

// defaultAgentType is the fallback used when the caller hasn't populated
// Outcome.AgentType — namely the pre-config WriteFailure paths in main,
// which fire before AGENT_TYPE has been read from the environment. The
// orchestrator (agent.Run) always sets AgentType from cfg.AgentType
// explicitly, so on the happy path this constant is unused.
const defaultAgentType = "claude-code"

// Exit codes per docs/CONTRACT.md.
const (
	ExitSuccess          = 0
	ExitExecutionFailure = 1
	ExitAuthFailure      = 2
	ExitCancelled        = 3
	ExitTimeout          = 4
)

// Status values per docs/CONTRACT.md.
type Status string

const (
	StatusSuccess   Status = "success"
	StatusFailure   Status = "failure"
	StatusCancelled Status = "cancelled"
	StatusTimeout   Status = "timeout"
)

// Outcome is the structured result of an agent run.
type Outcome struct {
	SchemaVersion  int        `json:"schema_version"`
	AgentType      string     `json:"agent_type"`
	AgentVersion   string     `json:"agent_version"`
	Status         Status     `json:"status"`
	ChangesSummary string     `json:"changes_summary"`
	FilesChanged   []string   `json:"files_changed"`
	TokenUsage     TokenUsage `json:"token_usage"`
	Turns          int        `json:"turns"`
	Error          string     `json:"error,omitempty"`

	// Model is the actual model identifier the agent reported using
	// (e.g., "claude-opus-4-7"). Captured from the agent's output
	// stream — for Claude Code, the system.init event of its
	// stream-json. Distinct from cfg.Model (the user-requested value),
	// which may be empty when the user picked the server-side default
	// or which Claude Code may have routed differently. Empty when the
	// agent never reported one (e.g., crashed before init).
	Model string `json:"model,omitempty"`

	// StartedAt is the unix-second timestamp captured just before the
	// agent subprocess was started. Pairs with EndedAt to make
	// wall-clock duration self-describing on the result file rather
	// than implicit in surrounding Job timestamps.
	StartedAt int64 `json:"started_at,omitempty"`

	// EndedAt is the unix-second timestamp captured when the agent
	// subprocess exited (any path: success, failure, cancelled,
	// timeout).
	EndedAt int64 `json:"ended_at,omitempty"`

	// DeniedHosts is the dedup-sorted list of hostnames the in-process
	// CONNECT proxy refused because they weren't on the agent's
	// allowlist. Surfaced so the consumer (the runner / dashboard) can
	// tell the user "your task tried to reach X but it was blocked —
	// add it to your allowlist if expected." Empty / omitted when no
	// denies happened. Other proxy deny categories (IP literal,
	// non-443 port, non-CONNECT method, private-IP block) are
	// deliberately not included; those represent agent bugs or
	// security-gate violations rather than allowlist gaps.
	DeniedHosts []string `json:"denied_hosts,omitempty"`

	// PRTitle is the agent-produced short title intended for use as the
	// pull request's title. The agent's system prompt asks for ≤72
	// chars (Conventional Commits + GitHub's PR title soft limit);
	// Write enforces a hard cap of prTitleMaxRunes runes with an
	// ellipsis suffix so consumers never see a runaway-length title
	// even when the agent ignores the instruction.
	//
	// Distinct from ChangesSummary, which is longer and describes
	// what + why. PRTitle is title-shaped (imperative mood, one line).
	PRTitle string `json:"pr_title,omitempty"`

	// ExitCode is the classified exit code (see Exit* constants:
	// 0 success, 1 generic execution failure, 2 auth/rate-limit,
	// 3 cancelled, 4 timeout). agentbox's own process exits with this
	// value AND emits it in result.json — the latter so consumers
	// (the deployment-runner) that ignore the container's wait exit
	// code in favour of result.json don't lose the failure-class
	// signal. Pre-v1.1.4 this field was `json:"-"` and the runner
	// could only see status; that gap caused real debugging pain on
	// the first Tasks dogfood deploy. ExitCode == 0 is meaningful in
	// the success path (paired with status="success"); on the failure
	// path it identifies which classifier hit.
	ExitCode int `json:"exit_code"`
}

type TokenUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
}

// Path returns the destination for result.json — $RESULT_PATH or the
// default. See docs/CONTRACT.md for bind-mount guidance.
func Path() string {
	if p := os.Getenv("RESULT_PATH"); p != "" {
		return p
	}
	return "/tmp/result.json"
}

// prTitleMaxRunes is the hard cap enforced on Outcome.PRTitle at Write
// time. Matches the soft cap requested in the agent's system prompt
// (Conventional Commits + GitHub's PR title soft limit). When the
// agent ignores the soft cap and emits a longer title, Write truncates
// and appends a single ellipsis rune so consumers never see a runaway
// title.
const prTitleMaxRunes = 72

// Write serializes the Outcome as JSON. SchemaVersion is overwritten;
// AgentType falls back to "claude-code" only when the caller didn't
// populate it (so pre-config WriteFailure paths still emit a value);
// PRTitle is hard-capped to prTitleMaxRunes runes; the rest of the
// fields pass through.
func Write(o Outcome) error {
	o.SchemaVersion = schemaVersion
	if o.AgentType == "" {
		o.AgentType = defaultAgentType
	}
	if o.FilesChanged == nil {
		o.FilesChanged = []string{}
	}
	o.PRTitle = capPRTitle(o.PRTitle)

	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(), append(data, '\n'), 0o644)
}

// capPRTitle enforces the prTitleMaxRunes cap with ellipsis. Operates
// on runes so multi-byte titles aren't cut mid-codepoint. Returns the
// input unchanged when already within budget or empty.
func capPRTitle(s string) string {
	if s == "" {
		return s
	}
	if utf8.RuneCountInString(s) <= prTitleMaxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:prTitleMaxRunes-1]) + "…"
}

// WriteFailure writes an Outcome for a pre-execution failure where the
// agent never ran. Summary is optional.
func WriteFailure(err error, summary string) error {
	return Write(Outcome{
		Status:         StatusFailure,
		ChangesSummary: summary,
		Error:          err.Error(),
		ExitCode:       ExitExecutionFailure,
	})
}

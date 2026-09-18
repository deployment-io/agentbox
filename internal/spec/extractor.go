// Package spec extracts the structured task-spec an interactive agent
// maintains across a conversation. The agent is instructed (via its
// appended system prompt) to keep a fenced ```task-spec``` block of JSON
// at the end of its messages; this package finds the latest valid block
// and parses it into an agent.SpecSnapshot.
//
// Agent-agnostic: the block convention is part of how we prompt any chat
// agent, not specific to Claude Code, so this lives outside the agent
// packages.
package spec

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/deployment-io/agentbox/internal/agent"
)

// fence is the Markdown code-fence delimiter. Defined as a const so the
// regex below can be an interpreted string literal (Go raw strings can't
// contain backticks).
const fence = "```"

// openRe matches an opening ```task-spec fence on a line of its own and
// closeRe a closing ``` fence on a line of its own. Both are anchored to
// the start of a line: a fence marker mentioned mid-sentence (the agent
// describing the block format in prose) must not open a block, or it
// swallows everything up to the real block's opening fence and the spec
// is lost — seen in production 2026-09-18.
var (
	openRe  = regexp.MustCompile("(?m)^" + fence + `task-spec[ \t]*$`)
	closeRe = regexp.MustCompile("(?m)^" + fence + `[ \t]*$`)
)

// span is one fenced block: [start, end) covers both fences; body is the
// text between them.
type span struct {
	start, end int
	body       string
}

// blocks returns every well-formed ```task-spec``` block in text, newest
// first. Openings are paired with the nearest closing fence that follows
// them, scanning from the last opening backwards so that an earlier
// unclosed opening (a prose mention at line start, or a block the agent
// never closed) can neither claim the newest block's closing fence nor
// mask the block itself. An opening with no closing fence after it is
// not a block.
func blocks(text string) []span {
	opens := openRe.FindAllStringIndex(text, -1)
	var out []span
	limit := len(text) // blocks must end before the previously found one starts
	for i := len(opens) - 1; i >= 0; i-- {
		o := opens[i]
		if o[1] >= limit {
			continue // this opening sits inside a block already claimed
		}
		rest := text[o[1]:limit]
		c := closeRe.FindStringIndex(rest)
		if c == nil {
			continue
		}
		out = append(out, span{
			start: o[0],
			end:   o[1] + c[1],
			body:  rest[:c[0]],
		})
		limit = o[0]
	}
	return out
}

// parsedSpec mirrors the JSON the agent emits inside the block. Field
// names track the system-prompt schema.
type parsedSpec struct {
	Title       string   `json:"title"`
	Goal        string   `json:"goal"`
	Context     string   `json:"context"`
	Acceptance  []string `json:"acceptance_criteria"`
	Assumptions []string `json:"assumptions"`
	OutOfScope  []string `json:"out_of_scope"`
	Readiness   string   `json:"readiness"`
	Notes       string   `json:"readiness_notes"`
	Complexity  string   `json:"complexity"`
}

// Extract returns the task-spec parsed from the latest valid
// ```task-spec``` block in text, and ok=true. It returns ok=false when no
// block is present, when no block parses as JSON, or when the latest
// parseable block carries no substance (no title and no goal). Blocks are
// scanned newest-first, so the most recent valid spec wins and an older
// one is used only as a fallback when newer blocks are malformed.
func Extract(text string) (agent.SpecSnapshot, bool) {
	for _, b := range blocks(text) {
		raw := strings.TrimSpace(b.body)
		var p parsedSpec
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			continue
		}
		// A content-free block ({} or whitespace) isn't a spec — keep
		// looking so it doesn't mask an earlier substantive one and so
		// the dashboard's "no spec yet" state stays accurate.
		if p.Title == "" && p.Goal == "" {
			continue
		}
		return agent.SpecSnapshot{
			Title:       p.Title,
			Goal:        p.Goal,
			Context:     p.Context,
			Acceptance:  p.Acceptance,
			Assumptions: p.Assumptions,
			OutOfScope:  p.OutOfScope,
			Readiness:   p.Readiness,
			Notes:       p.Notes,
			Complexity:  p.Complexity,
			Raw:         raw,
		}, true
	}
	return agent.SpecSnapshot{}, false
}

// Strip removes all ```task-spec``` blocks from text and trims surrounding
// whitespace, leaving the user-facing prose. Used to keep the machine-only
// spec block out of the rendered chat message.
func Strip(text string) string {
	// blocks is newest-first, so removing in order keeps earlier offsets valid.
	for _, b := range blocks(text) {
		text = text[:b.start] + text[b.end:]
	}
	return strings.TrimSpace(text)
}

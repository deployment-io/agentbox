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

// specBlockRe matches a fenced ```task-spec ... ``` block and captures
// its inner body. Dotall so the JSON can span lines; non-greedy so the
// first closing fence ends a block.
var specBlockRe = regexp.MustCompile("(?s)" + fence + `task-spec\s*\n(.*?)` + fence)

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
}

// Extract returns the task-spec parsed from the latest valid
// ```task-spec``` block in text, and ok=true. It returns ok=false when no
// block is present, when no block parses as JSON, or when the latest
// parseable block carries no substance (no title and no goal). Blocks are
// scanned newest-first, so the most recent valid spec wins and an older
// one is used only as a fallback when newer blocks are malformed.
func Extract(text string) (agent.SpecSnapshot, bool) {
	matches := specBlockRe.FindAllStringSubmatch(text, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		raw := strings.TrimSpace(matches[i][1])
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
			Raw:         raw,
		}, true
	}
	return agent.SpecSnapshot{}, false
}

// Strip removes all ```task-spec``` blocks from text and trims surrounding
// whitespace, leaving the user-facing prose. Used to keep the machine-only
// spec block out of the rendered chat message.
func Strip(text string) string {
	return strings.TrimSpace(specBlockRe.ReplaceAllString(text, ""))
}

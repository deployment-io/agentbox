// Package reposuggestion extracts the repository suggestions an interactive
// agent emits while planning: repositories that are not checked out into the
// session but that the outcome actually needs. The agent is instructed (via its
// appended system prompt) to emit at most one
// <repo-suggestion>…</repo-suggestion> block of JSON per message; this package
// finds the latest valid block, parses it into an agent.RepoSuggestion, and
// strips every block from the display text.
//
// Sibling to internal/spec and agent-agnostic for the same reason: the block
// convention is part of how we prompt any chat agent, not specific to one.
//
// The payload is DISPLAY DATA ONLY. A suggested name is a lookup key the
// consumer resolves against the org's repository list — nothing here is ever
// used as a clone URL, provider or branch.
package reposuggestion

import (
	"encoding/json"
	"strings"

	"github.com/deployment-io/agentbox/internal/agent"
)

// Caps applied at extraction. The block is model output, so it is bounded here
// rather than downstream: a suggestion that names half the org, or carries an
// essay as a reason, is a malfunctioning turn, not a useful card. The server
// re-applies the same caps — it does not trust the runner's payload.
const (
	// MaxRepositories is how many suggested repositories survive one block.
	MaxRepositories = 5
	// MaxNameRunes bounds a suggested repository name ("org/repo").
	MaxNameRunes = 200
	// MaxReasonRunes bounds the one-line reason shown under a name.
	MaxReasonRunes = 300
)

const (
	openTag  = "<repo-suggestion>"
	closeTag = "</repo-suggestion>"
)

// span is one tagged block: [start, end) covers both tags; body is the text
// between them.
type span struct {
	start, end int
	body       string
}

// blocks returns every well-formed <repo-suggestion> block in text, newest
// first. Openings are paired with the nearest closing tag that follows them,
// scanning from the last opening backwards, so an earlier unclosed opening (the
// agent naming the tag in prose, or a block it never closed) can neither claim
// the newest block's closing tag nor mask the block itself — the same hazard
// internal/spec hit in production with a prose-mentioned fence. An opening with
// no closing tag after it is not a block.
func blocks(text string) []span {
	var out []span
	limit := len(text) // a block must end before the previously found one starts
	for {
		o := strings.LastIndex(text[:limit], openTag)
		if o < 0 {
			return out
		}
		rest := text[o+len(openTag) : limit]
		if c := strings.Index(rest, closeTag); c >= 0 {
			out = append(out, span{
				start: o,
				end:   o + len(openTag) + c + len(closeTag),
				body:  rest[:c],
			})
		}
		limit = o
	}
}

// parsedSuggestion mirrors the JSON the agent emits inside the block. Field
// names track the system-prompt schema.
type parsedSuggestion struct {
	Repositories []parsedRepo `json:"repositories"`
}

type parsedRepo struct {
	Name       string `json:"name"`
	Reason     string `json:"reason"`
	Confidence string `json:"confidence"`
}

// Extract returns the repository suggestion parsed from the latest valid
// <repo-suggestion> block in text, and ok=true. It returns ok=false when no
// block is present, when no block parses as JSON, or when the latest parseable
// block yields no usable repository (every entry missing a name). Blocks are
// scanned newest-first, so the most recent valid suggestion wins and an older
// one is used only as a fallback when newer blocks are malformed.
func Extract(text string) (agent.RepoSuggestion, bool) {
	for _, b := range blocks(text) {
		var p parsedSuggestion
		if err := json.Unmarshal([]byte(strings.TrimSpace(b.body)), &p); err != nil {
			continue
		}
		repos := capRepositories(p.Repositories)
		// A block that names nothing isn't a suggestion — keep looking so it
		// doesn't mask an earlier substantive one, and so a consumer's "no
		// suggestion yet" state stays accurate.
		if len(repos) == 0 {
			continue
		}
		return agent.RepoSuggestion{Repositories: repos}, true
	}
	return agent.RepoSuggestion{}, false
}

// capRepositories drops entries without a name, truncates name and reason to
// their rune caps, normalises confidence, and keeps at most MaxRepositories.
func capRepositories(in []parsedRepo) []agent.SuggestedRepo {
	var out []agent.SuggestedRepo
	for _, r := range in {
		name := strings.TrimSpace(r.Name)
		if name == "" {
			continue
		}
		out = append(out, agent.SuggestedRepo{
			Name:       truncateRunes(name, MaxNameRunes),
			Reason:     truncateRunes(strings.TrimSpace(r.Reason), MaxReasonRunes),
			Confidence: normalizeConfidence(r.Confidence),
		})
		if len(out) == MaxRepositories {
			break
		}
	}
	return out
}

// normalizeConfidence maps the agent's confidence onto the three values a
// consumer renders; anything unrecognised (including an absent one) reads as
// "medium", the neutral middle.
func normalizeConfidence(c string) string {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "high":
		return "high"
	case "low":
		return "low"
	default:
		return "medium"
	}
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// Strip removes all <repo-suggestion> blocks from text and trims surrounding
// whitespace, leaving the user-facing prose. Used to keep the machine-only
// block out of the rendered chat message.
func Strip(text string) string {
	// blocks is newest-first, so removing in order keeps earlier offsets valid.
	for _, b := range blocks(text) {
		text = text[:b.start] + text[b.end:]
	}
	return strings.TrimSpace(text)
}

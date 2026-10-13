// Package qa turns a Task's spec into a QA run: it finds the acceptance
// criteria, builds the prompt that has an agent check them on live
// static-site previews through the runner's preview tools, and lifts the
// agent's <qa> trailer back out of its final message.
//
// A QA run is BLIND. Nothing here computes a diff, reads a repository or
// looks at what an implement run said about its work: the prompt is the
// criteria, the repository names to preview and the rules, and nothing else.
//
// Sibling to internal/review, internal/spec and internal/reposuggestion, and
// it follows their machine-owned-block rules exactly: the tag is matched on
// its own line, the newest valid block wins, and EVERY block is stripped from
// the display text.
package qa

import (
	"encoding/json"
	"strings"
)

// Caps on the criteria, matching the stored QA result. The runner's own caps
// on its acceptance list (20 × 300) are for the PR body's display list only;
// a QA result must be able to account for every criterion it was given.
const (
	MaxCriteria       = 50
	MaxCriterionRunes = 500
)

// rerunPrefix opens the re-run instructions the runner prepends to a spec on
// a re-run; specSeparator introduces the spec itself after them.
const (
	rerunPrefix   = "[Re-run instructions"
	specSeparator = "\n[Spec]\n"
)

// Criteria returns the acceptance criteria in spec, found the way the
// runner's parseAcceptanceCriteria finds them, and the re-run instructions
// block when spec carries one.
//
// A spec that starts with the re-run prefix is split at the LAST "[Spec]"
// line: what comes before it is the instructions, what follows is the spec.
// A spec that is a JSON object contributes its "Acceptance" list (matched
// case-insensitively, as the runner's untagged TaskSpec is); prose has no
// criteria. Each criterion is collapsed to one line, blanks are dropped, and
// the caps above apply.
func Criteria(spec string) (criteria []string, instructions string) {
	body := strings.TrimSpace(spec)
	if strings.HasPrefix(body, rerunPrefix) {
		if i := strings.LastIndex(body, specSeparator); i >= 0 {
			instructions = strings.TrimSpace(body[:i])
			body = strings.TrimSpace(body[i+len(specSeparator):])
		} else {
			// Instructions with no spec after them: nothing to take criteria
			// from.
			return nil, body
		}
	}
	return acceptance(body), instructions
}

// acceptance reads a JSON object's "Acceptance" list. Anything that is not a
// JSON object with a list of strings there has no criteria.
func acceptance(body string) []string {
	if !strings.HasPrefix(body, "{") {
		return nil
	}
	var spec struct {
		Acceptance []string
	}
	if err := json.Unmarshal([]byte(body), &spec); err != nil {
		return nil
	}
	var out []string
	for _, c := range spec.Acceptance {
		line := strings.Join(strings.Fields(c), " ")
		if line == "" {
			continue
		}
		out = append(out, truncateRunes(line, MaxCriterionRunes))
		if len(out) == MaxCriteria {
			break
		}
	}
	return out
}

// truncateRunes cuts s to at most max RUNES, so a cut never splits a
// multi-byte character.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

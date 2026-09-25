// Package review turns an implement run's diff into a review: it computes
// what changed against each repository's start-of-run commit, decides which
// focused passes are worth spending a model call on, builds the prompt, and
// lifts the agent's <review> trailer back out of its final message.
//
// Sibling to internal/spec and internal/reposuggestion, and it follows their
// machine-owned-block rules exactly: the tag is matched on its own line,
// openings are paired with the nearest following close scanning newest-first
// so a prose mention cannot swallow a real block, the latest valid block wins,
// and EVERY block is stripped from the display text — a <review> block that
// leaked into changes_summary would be rendered verbatim into a PR body.
package review

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
)

// Finding, Coverage and Previous are the result-package types under this
// package's own names. One definition, two readable call sites: the extractor
// and the pass selector both talk about findings and coverage, and result owns
// the shape because result owns the file they are written to.
type (
	Finding  = result.ReviewFinding
	Coverage = result.ReviewCoverage
	Previous = result.PreviousFinding
)

// Caps applied AT EXTRACTION, mirroring kit's review_models by hand.
//
// A deliberate hand-mirror, like the runner's tokenUsage struct: agentbox is a
// standalone module and cannot import the constants. The numbers must stay in
// step with review_models — kit re-applies them at the trust boundary and does
// not trust this payload, so a drift here costs truncation twice rather than
// corruption, which is why a hand-mirror is an acceptable trade.
//
// Every cap TRUNCATES, never rejects. A review that found 400 things is still
// worth its first 100, and a finding whose Why runs to a thousand lines is
// still a real finding.
const (
	MaxFindings       = 100
	MaxKeyRunes       = 120
	MaxLocationRunes  = 400
	MaxWhatRunes      = 1000
	MaxWhyRunes       = 1000
	MaxPassRunes      = 60
	MaxNoteRunes      = 400
	MaxReasonRunes    = 400
	MaxParameterRunes = 60
	MaxSeverityRunes  = 60
	MaxStateRunes     = 60
)

// StageReview is the stage every finding from a review run carries. Stamped by
// agentbox on extraction, never read from the agent's trailer.
const StageReview = "review"

const (
	openTag  = "<review>"
	closeTag = "</review>"
)

// openRe / closeRe match a tag ON A LINE OF ITS OWN. Anchoring matters: a
// mid-sentence mention of the tag (an agent explaining the format it was asked
// for) would otherwise open a block and swallow everything up to the real
// block's opening — the failure internal/spec hit in production on 2026-09-18.
var (
	openRe  = regexp.MustCompile(`(?m)^[ \t]*` + openTag + `[ \t]*$`)
	closeRe = regexp.MustCompile(`(?m)^[ \t]*` + closeTag + `[ \t]*$`)
)

// span is one tagged block: [start, end) covers both tags; body is between.
type span struct {
	start, end int
	body       string
}

// blocks returns every well-formed <review> block in text, NEWEST FIRST.
// Openings are paired with the nearest closing tag that follows them, scanning
// from the last opening backwards, so an earlier unclosed opening can neither
// claim the newest block's closing tag nor mask the block itself.
func blocks(text string) []span {
	opens := openRe.FindAllStringIndex(text, -1)
	var out []span
	limit := len(text) // a block must end before the previously found one starts
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
		out = append(out, span{start: o[0], end: o[1] + c[1], body: rest[:c[0]]})
		limit = o[0]
	}
	return out
}

// parsedTrailer mirrors the JSON the agent emits inside the block.
type parsedTrailer struct {
	Findings []parsedFinding  `json:"findings"`
	Coverage []parsedCovered  `json:"coverage"`
	Previous []parsedPrevious `json:"previous"`
}

type parsedFinding struct {
	Key       string `json:"key"`
	Parameter string `json:"parameter"`
	Severity  string `json:"severity"`
	Location  string `json:"location"`
	What      string `json:"what"`
	Why       string `json:"why"`
	Pass      string `json:"pass"`
	// Stage is accepted off the wire and then DISCARDED. Declaring it is how
	// the discarding is visible: an agent that emits one must not be able to
	// relabel where its finding came from.
	Stage string `json:"stage"`
}

type parsedCovered struct {
	Parameter string `json:"parameter"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
}

type parsedPrevious struct {
	Key    string `json:"key"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

// The two statuses a previously reported finding can come back with. Anything
// else is dropped rather than guessed at: an unreadable status that survived
// would have to be read as one of the two, and reading it as "resolved" is the
// direction that clears a must-fix nobody fixed.
const (
	StatusResolved     = "resolved"
	StatusStillPresent = "still_present"
)

// Extract returns the findings, the agent's own coverage claims and its status
// for each previously reported finding from the latest valid <review> block in
// text, plus the text with every block removed.
//
// open is the list of findings the round was GIVEN (REVIEW_OPEN_FINDINGS); it
// bounds the status list, because a status for a key nobody asked about says
// nothing about the change under review.
//
// ok=false means no block parsed — the caller still gets the stripped text, so
// a malformed trailer can never leak into a PR body. Blocks are scanned
// newest-first, so the most recent valid one wins and an older block is a
// fallback only when the newer ones are malformed.
func Extract(text string, open []config.ReviewOpenFinding) (findings []Finding, claimed []Coverage, previous []Previous, stripped string, ok bool) {
	stripped = Strip(text)
	for _, b := range blocks(text) {
		var parsed parsedTrailer
		if err := json.Unmarshal([]byte(strings.TrimSpace(b.body)), &parsed); err != nil {
			continue
		}
		return capFindings(parsed.Findings), capCoverage(parsed.Coverage), capPrevious(parsed.Previous, open), stripped, true
	}
	return nil, nil, nil, stripped, false
}

// capPrevious bounds the status list to the findings that were actually given:
// at most one entry per given key, at most as many entries as keys, a status
// that is one of the two names, and a bounded note.
//
// Everything here drops rather than repairs. A status for an unknown key, a
// second status for a key already answered, or a status word this contract
// does not define would all have to be interpreted to be kept, and every
// interpretation runs the risk of reading "the problem is gone" into something
// that did not say so. A dropped entry is an ABSENT entry, and absence is
// already defined as still present — the safe direction.
func capPrevious(in []parsedPrevious, open []config.ReviewOpenFinding) []Previous {
	if len(open) == 0 || len(in) == 0 {
		return nil
	}
	given := make(map[string]bool, len(open))
	for _, f := range open {
		given[truncateRunes(strings.TrimSpace(f.Key), MaxKeyRunes)] = true
	}
	answered := map[string]bool{}
	var out []Previous
	for _, p := range in {
		key := truncateRunes(strings.TrimSpace(p.Key), MaxKeyRunes)
		if !given[key] || answered[key] {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(p.Status))
		if status != StatusResolved && status != StatusStillPresent {
			continue
		}
		answered[key] = true
		out = append(out, Previous{
			Key:    key,
			Status: status,
			Note:   truncateRunes(strings.TrimSpace(p.Note), MaxNoteRunes),
		})
		if len(out) == len(open) {
			break
		}
	}
	return out
}

// Strip removes every <review> block from text and trims the result. Called on
// every path, including the ones where nothing parsed: the block is machine
// payload, and the display text is what a human reads in the PR body.
func Strip(text string) string {
	// blocks is newest-first, so removing in order keeps earlier offsets valid.
	for _, b := range blocks(text) {
		text = text[:b.start] + text[b.end:]
	}
	return strings.TrimSpace(text)
}

// capFindings applies the caps and stamps the stage. A finding with no what
// and no location says nothing a reader can act on, so it is dropped rather
// than carried as an empty row.
//
// Returns an EMPTY SLICE, never nil, so "the review ran and found nothing"
// serialises as `"findings": []` rather than `"findings": null`. The two mean
// the same thing to a Go consumer and different things to everyone else.
func capFindings(in []parsedFinding) []Finding {
	out := []Finding{}
	for _, f := range in {
		what := strings.TrimSpace(f.What)
		location := strings.TrimSpace(f.Location)
		if what == "" && location == "" {
			continue
		}
		out = append(out, Finding{
			Key:       truncateRunes(strings.TrimSpace(f.Key), MaxKeyRunes),
			Parameter: truncateRunes(strings.TrimSpace(f.Parameter), MaxParameterRunes),
			Severity:  truncateRunes(strings.TrimSpace(f.Severity), MaxSeverityRunes),
			Location:  truncateRunes(location, MaxLocationRunes),
			What:      truncateRunes(what, MaxWhatRunes),
			Why:       truncateRunes(strings.TrimSpace(f.Why), MaxWhyRunes),
			// Stamped, never taken from f.Stage.
			Stage: StageReview,
			Pass:  truncateRunes(strings.TrimSpace(f.Pass), MaxPassRunes),
		})
		if len(out) == MaxFindings {
			break
		}
	}
	return out
}

// capCoverage bounds the agent's coverage claims: one entry per parameter
// (first claim wins — a parameter reported twice is a producer bug, not extra
// information) and a bounded reason.
func capCoverage(in []parsedCovered) []Coverage {
	seen := map[string]bool{}
	var out []Coverage
	for _, c := range in {
		parameter := strings.TrimSpace(c.Parameter)
		if parameter == "" || seen[strings.ToLower(parameter)] {
			continue
		}
		seen[strings.ToLower(parameter)] = true
		out = append(out, Coverage{
			Parameter: truncateRunes(parameter, MaxParameterRunes),
			State:     truncateRunes(strings.TrimSpace(c.State), MaxStateRunes),
			Reason:    truncateRunes(strings.TrimSpace(c.Reason), MaxReasonRunes),
		})
		if len(out) == len(allParameters) {
			break
		}
	}
	return out
}

// truncateRunes cuts s to at most max RUNES. Cutting bytes would split a
// multi-byte character and leave invalid UTF-8 in a field that goes on to be
// rendered in a PR body.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		// len is a byte count and an upper bound on the rune count: if the
		// bytes fit, the runes certainly do.
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

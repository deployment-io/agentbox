package qa

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
	"github.com/deployment-io/agentbox/internal/review"
)

// Caps applied at extraction. Findings use the review extractor's caps
// (review.Max*), because they are Review-shaped findings.
const (
	MaxReasonRunes      = 400
	MaxObservations     = 20
	MaxObservationRunes = 300
)

// StageQA is the stage every QA finding carries and PassCriteria its pass.
// Both are stamped by agentbox, never read from the trailer.
const (
	StageQA      = "qa"
	PassCriteria = "criteria"
)

// Reasons agentbox writes itself.
const (
	ReasonNotReported = "QA didn't report on this criterion"
	ReasonNoResult    = "QA didn't report a result"
	reasonNoReason    = "QA gave no reason"
)

const (
	openTag  = "<qa>"
	closeTag = "</qa>"
)

// openRe / closeRe match a tag ON A LINE OF ITS OWN, so a prose mention of the
// tag cannot open a block — see internal/review.
var (
	openRe  = regexp.MustCompile(`(?m)^[ \t]*` + openTag + `[ \t]*$`)
	closeRe = regexp.MustCompile(`(?m)^[ \t]*` + closeTag + `[ \t]*$`)
)

type span struct {
	start, end int
	body       string
}

// blocks returns every well-formed <qa> block in text, NEWEST FIRST, pairing
// each opening with the nearest following close, scanning from the last
// opening backwards (the internal/review algorithm).
func blocks(text string) []span {
	opens := openRe.FindAllStringIndex(text, -1)
	var out []span
	limit := len(text)
	for i := len(opens) - 1; i >= 0; i-- {
		o := opens[i]
		if o[1] >= limit {
			continue
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

// Strip removes every <qa> block from text and trims the result. Called on
// every path: the block is machine payload, and the display text can end up
// in a PR body.
func Strip(text string) string {
	for _, b := range blocks(text) {
		text = text[:b.start] + text[b.end:]
	}
	return strings.TrimSpace(text)
}

type parsedTrailer struct {
	Status       string            `json:"status"`
	StatusReason string            `json:"status_reason"`
	Criteria     []parsedCriterion `json:"criteria"`
	Findings     []parsedFinding   `json:"findings"`
	Observations []string          `json:"observations"`
}

type parsedCriterion struct {
	Index  flexInt `json:"index"`
	State  string  `json:"state"`
	Reason string  `json:"reason"`
}

type parsedFinding struct {
	Key       string `json:"key"`
	Parameter string `json:"parameter"`
	Severity  string `json:"severity"`
	Location  string `json:"location"`
	What      string `json:"what"`
	Why       string `json:"why"`
}

// flexInt accepts a JSON number or a numeric string, so an index written as
// "2" does not discard the whole trailer. Anything else is 0, which is out of
// range and ignored.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := string(bytes.Trim(bytes.TrimSpace(b), `"`))
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexInt(n)
	return nil
}

// latestTrailer parses the newest <qa> block that is valid JSON.
func latestTrailer(text string) (parsedTrailer, bool) {
	for _, b := range blocks(text) {
		var parsed parsedTrailer
		if err := json.Unmarshal([]byte(strings.TrimSpace(b.body)), &parsed); err != nil {
			continue
		}
		return parsed, true
	}
	return parsedTrailer{}, false
}

// Lift returns the QA result for finalMessage and the message with every <qa>
// block stripped.
//
// agentbox, not the agent, decides the shape: every criterion parsed from
// cfg.QASpec appears exactly once, in spec order, with agentbox's own text;
// one the trailer does not mention is not_checked; an out-of-range or
// duplicate index is ignored; unknown states are not_checked; every finding
// is stamped stage "qa" and pass "criteria". No valid trailer means
// couldnt_run.
func Lift(cfg *config.Config, finalMessage string) (*result.QAResult, string) {
	criteria, _ := Criteria(cfg.QASpec)
	stripped := Strip(finalMessage)
	parsed, ok := latestTrailer(finalMessage)

	out := &result.QAResult{
		NoCriteria:   len(criteria) == 0,
		Criteria:     []result.QACriterion{},
		Findings:     []result.ReviewFinding{},
		Observations: []string{},
	}
	if !ok {
		out.Status = result.QAStatusCouldntRun
		out.StatusReason = ReasonNoResult
		for _, c := range criteria {
			out.Criteria = append(out.Criteria, result.QACriterion{Criterion: c, State: result.QAStateNotChecked, Reason: ReasonNoResult})
		}
		return out, stripped
	}

	out.Status, out.StatusReason = liftStatus(parsed.Status, parsed.StatusReason)
	out.Criteria = liftCriteria(criteria, parsed.Criteria)
	out.Findings = liftFindings(parsed.Findings)
	out.Observations = liftObservations(parsed.Observations)
	return out, stripped
}

// normalizeWord lowercases a status or state and folds spaces and hyphens
// into underscores, so "Not checked" and "couldnt-run" read as the wire value.
func normalizeWord(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer(" ", "_", "-", "_", "'", "").Replace(s)
	return s
}

func liftStatus(status, reason string) (string, string) {
	reason = truncateRunes(strings.TrimSpace(reason), MaxReasonRunes)
	switch s := normalizeWord(status); s {
	case result.QAStatusCompleted:
		return s, reason
	case result.QAStatusCouldntRun:
		if reason == "" {
			reason = reasonNoReason
		}
		return s, reason
	default:
		// A status this contract does not define cannot be read as
		// "completed": that is the claim that the criteria were checked.
		// "skipped" lands here too: it is the runner's word for a change
		// with no static site to QA, decided before any QA run is spawned,
		// so an agent that ran and reports it is reporting something else.
		return result.QAStatusCouldntRun, truncateRunes(fmt.Sprintf("QA reported an unrecognised status %q", status), MaxReasonRunes)
	}
}

func liftCriteria(criteria []string, reported []parsedCriterion) []result.QACriterion {
	byIndex := map[int]parsedCriterion{}
	for _, r := range reported {
		i := int(r.Index)
		if i < 1 || i > len(criteria) {
			continue
		}
		if _, dup := byIndex[i]; dup {
			continue // first report wins; a second is a producer bug
		}
		byIndex[i] = r
	}
	out := make([]result.QACriterion, 0, len(criteria))
	for i, text := range criteria {
		r, ok := byIndex[i+1]
		if !ok {
			out = append(out, result.QACriterion{Criterion: text, State: result.QAStateNotChecked, Reason: ReasonNotReported})
			continue
		}
		state, reason := liftState(r.State, r.Reason)
		out = append(out, result.QACriterion{Criterion: text, State: state, Reason: reason})
	}
	return out
}

func liftState(state, reason string) (string, string) {
	reason = truncateRunes(strings.TrimSpace(reason), MaxReasonRunes)
	switch s := normalizeWord(state); s {
	case result.QAStatePassed, result.QAStateFailed:
		return s, reason
	case result.QAStateNotChecked:
		if reason == "" {
			reason = reasonNoReason
		}
		return s, reason
	default:
		if reason == "" {
			reason = truncateRunes(fmt.Sprintf("QA reported an unrecognised state %q", state), MaxReasonRunes)
		}
		return result.QAStateNotChecked, reason
	}
}

// liftFindings caps findings with the review extractor's caps and stamps the
// stage and pass. A finding with no what and no location is dropped.
func liftFindings(in []parsedFinding) []result.ReviewFinding {
	out := []result.ReviewFinding{}
	for _, f := range in {
		what := strings.TrimSpace(f.What)
		location := strings.TrimSpace(f.Location)
		if what == "" && location == "" {
			continue
		}
		out = append(out, result.ReviewFinding{
			Key:       truncateRunes(strings.TrimSpace(f.Key), review.MaxKeyRunes),
			Parameter: truncateRunes(strings.TrimSpace(f.Parameter), review.MaxParameterRunes),
			Severity:  truncateRunes(strings.TrimSpace(f.Severity), review.MaxSeverityRunes),
			Location:  truncateRunes(location, review.MaxLocationRunes),
			What:      truncateRunes(what, review.MaxWhatRunes),
			Why:       truncateRunes(strings.TrimSpace(f.Why), review.MaxWhyRunes),
			Stage:     StageQA,
			Pass:      PassCriteria,
		})
		if len(out) == review.MaxFindings {
			break
		}
	}
	return out
}

func liftObservations(in []string) []string {
	out := []string{}
	for _, o := range in {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		out = append(out, truncateRunes(o, MaxObservationRunes))
		if len(out) == MaxObservations {
			break
		}
	}
	return out
}

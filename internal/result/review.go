package result

// The review half of the result.json schema — see docs/CONTRACT.md.
//
// WHAT IS DELIBERATELY ABSENT MATTERS AS MUCH AS WHAT IS HERE. There is no
// must_fix_open field anywhere in this file, and there must not be one: that
// is the decision about whether the work may proceed, and an agent that could
// emit it could wave its own findings through. The runner computes it from the
// findings and the org's thresholds, which agentbox never sees.
//
// Stage is here but is NOT the agent's to set either — agentbox stamps it (see
// internal/review.LiftResult), so an agent claiming a finding came from some
// other stage cannot make it so.

// ReviewFinding is one thing a review pass wants a human to know about the
// diff.
//
// Parameter, Severity and State are STRINGS across this contract. agentbox is
// a standalone module: it imports neither kit nor deployment-runner-kit, so it
// has no access to their numbered enums, and a number it invented would be a
// third numbering to keep in step. The consumer parses the name (case, spaces,
// hyphens and underscores ignored) and drops what it cannot read.
type ReviewFinding struct {
	Key       string `json:"key,omitempty"`
	Parameter string `json:"parameter,omitempty"`
	Severity  string `json:"severity,omitempty"`
	Location  string `json:"location,omitempty"`
	What      string `json:"what,omitempty"`
	Why       string `json:"why,omitempty"`
	// Stage is written by agentbox, always "review" for a review run,
	// regardless of what the agent put in its trailer.
	Stage string `json:"stage,omitempty"`
	Pass  string `json:"pass,omitempty"`
}

// ReviewCoverage records what happened to one review parameter: a pass ran
// (checked), a pass stood down (skipped, with a reason), or this release ships
// no pass for it (not checked).
//
// Without it, "no findings" is ambiguous in the worst direction — a parameter
// nothing ran for looks exactly like one that came back clean.
type ReviewCoverage struct {
	Parameter string `json:"parameter,omitempty"`
	State     string `json:"state,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// PreviousFinding is the reviewer's verdict on ONE finding a previous round
// left open and the runner handed back in (REVIEW_OPEN_FINDINGS).
//
// Key is echoed from the list that was given; a status for a key nobody asked
// about is dropped at extraction, so the reviewer cannot rename a problem into
// a different one. Status is "resolved" or "still_present" and nothing else.
//
// ABSENCE MEANS STILL PRESENT. A finding that was given and comes back with no
// entry is not resolved — it is unanswered, and the consumer treats unanswered
// and still present the same way. Silence must never clear a must-fix.
type PreviousFinding struct {
	Key    string `json:"key"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// ReviewResult is one review run's output: what it found, what it looked at,
// and — when the round was given the previous round's open findings — what
// became of each of those. All three lists are owned by agentbox after
// extraction: findings are capped and stage-stamped, coverage is rebuilt from
// what actually ran rather than taken from the agent, and a previous-status
// entry survives only when it names a key that was actually given.
type ReviewResult struct {
	Findings []ReviewFinding  `json:"findings"`
	Coverage []ReviewCoverage `json:"coverage"`

	// Previous is omitted entirely when the round was given no open findings
	// — the round-1 shape, where there is nothing to report a status for.
	Previous []PreviousFinding `json:"previous,omitempty"`
}

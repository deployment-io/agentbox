package review

import (
	"fmt"
	"strings"

	"github.com/deployment-io/agentbox/internal/config"
	"github.com/deployment-io/agentbox/internal/result"
)

// Plan is everything a review round needs before the agent starts: the prompt
// to run, the passes that will run, and the coverage record that describes
// what was and was not examined.
//
// Coverage is built BEFORE the agent runs, from facts agentbox knows —
// which passes the gate stood down and why, and whether the diff was
// truncated — rather than from what the agent says afterwards. A reviewer
// asked to report its own coverage will report that it covered everything.
type Plan struct {
	Prompt   string
	Passes   []string
	Coverage []Coverage
	Diff     Diff
}

// NothingToReview reports whether the cost gate stood every pass down, so the
// caller can skip spawning an agent entirely. The coverage record still
// explains why — a skipped review is a recorded decision, not a silence.
func (p Plan) NothingToReview() bool {
	return len(p.Passes) == 0
}

// Build computes the diff, applies the cost gate, and assembles the prompt.
func Build(cfg *config.Config) Plan {
	diff := Compute(cfg.WorkDir, cfg.ReviewBaseCommits)
	passes, skipped := SelectPasses(cfg.ReviewPasses, diff)
	plan := Plan{
		Passes:   passes,
		Coverage: BuildCoverage(passes, skipped, diff.Truncated),
		Diff:     diff,
	}
	if !plan.NothingToReview() {
		plan.Prompt = buildPrompt(cfg, plan)
	}
	return plan
}

// LiftResult turns the agent's final message into the review half of
// result.json, and returns the message with every <review> block removed.
//
// The coverage that SHIPS is agentbox's, not the agent's. The agent's claims
// are read and used only where they can make the record more honest: a pass
// that ran but reports itself skipped is believed, because an agent saying "I
// could not check this" is information nobody else has. A pass that ran and
// claims a cleaner state than agentbox recorded is not — that direction is the
// one that hides a gap.
func LiftResult(finalMessage string, planned []Coverage) (*result.ReviewResult, string) {
	findings, claimed, stripped, ok := Extract(finalMessage)
	if !ok {
		// No parseable trailer. The coverage record still goes out: the run
		// happened, the passes ran, and the consumer needs to know what was
		// examined even when the report came back unreadable.
		return &result.ReviewResult{Findings: []Finding{}, Coverage: planned}, stripped
	}
	return &result.ReviewResult{
		Findings: findings,
		Coverage: reconcileCoverage(planned, claimed),
	}, stripped
}

// reconcileCoverage merges the agent's claims into agentbox's record, one
// parameter at a time, and only ever in the direction that admits a gap.
func reconcileCoverage(planned, claimed []Coverage) []Coverage {
	byParameter := map[string]Coverage{}
	for _, c := range claimed {
		byParameter[normalize(c.Parameter)] = c
	}
	out := make([]Coverage, 0, len(planned))
	for _, p := range planned {
		claim, hasClaim := byParameter[normalize(p.Parameter)]
		if p.State == stateChecked && hasClaim && normalize(claim.State) == normalize(stateSkipped) {
			reason := claim.Reason
			if reason == "" {
				reason = "the pass reported that it could not check this parameter"
			}
			if p.Reason != "" {
				reason = reason + "; " + p.Reason
			}
			out = append(out, Coverage{Parameter: p.Parameter, State: stateSkipped, Reason: truncateRunes(reason, MaxReasonRunes)})
			continue
		}
		out = append(out, p)
	}
	return out
}

func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// passBriefs say what each pass is looking for. One focused brief per pass is
// the point of the design: an omnibus "review this diff" prompt returns a
// scattering of style notes, while a pass that has been told it is looking for
// one class of problem finds that class.
var passBriefs = map[string]string{
	PassSecurity:    `Look ONLY for security problems this diff introduces or leaves open: injection (SQL, command, template), missing authentication or authorisation on a new path, secrets or credentials committed or logged, unsafe deserialisation, path traversal, SSRF, missing validation of untrusted input crossing a trust boundary, a dependency change that pulls in something unvetted, and weakened crypto or transport security. Report a finding only when you can point at the line in the diff that causes it.`,
	PassCorrectness: `Look ONLY for correctness problems this diff introduces: logic that does not do what the surrounding code and the spec say it should, off-by-one and boundary errors, nil or null dereferences, unhandled errors and swallowed failures, race conditions and unsynchronised shared state, resource leaks, and behaviour changes the spec did not ask for. Report a finding only when you can point at the line in the diff that causes it, and say what input or state makes it go wrong.`,
}

// buildPrompt assembles the review prompt: the spec, the diff, one focused
// brief per pass, and the trailer format.
//
// It contains the diff, the spec and the pass list AND NOTHING ELSE. No
// previous result.json, no progress file, no transcript, no earlier round's
// findings: a reviewer shown last round's verdict grades against it instead of
// against the code. The runner enforces the same boundary structurally by
// moving the implementer's output directory out of the work dir before the
// round starts, so this is belt and braces rather than the only guard.
func buildPrompt(cfg *config.Config, plan Plan) string {
	var b strings.Builder
	b.WriteString("You are reviewing a code change. You are NOT implementing anything: do not edit, create or delete any file, and do not run any command that changes the working tree.\n\n")
	if cfg.ReviewRound > 1 {
		b.WriteString(fmt.Sprintf("This is review round %d. The diff below is the CURRENT state of the change, including fixes made since the previous round. Judge what you see now; you have not been shown the earlier round's findings and should not try to reconstruct them.\n\n", cfg.ReviewRound))
	}
	b.WriteString("[What the change is meant to achieve]\n")
	if strings.TrimSpace(cfg.ReviewSpec) != "" {
		b.WriteString(cfg.ReviewSpec)
	} else {
		b.WriteString("(no spec was supplied — judge the change on its own terms)")
	}
	b.WriteString("\n\n[The change under review]\n")
	b.WriteString("Each section below is one repository's diff against the commit it was checked out at when the Step began. Elision markers say where content was dropped.\n")
	b.WriteString(plan.Diff.Text)
	b.WriteString("\n\n[Passes]\n")
	b.WriteString("Run these passes ONE AT A TIME, in order. Each is a separate, focused examination of the same diff — finish one before starting the next, and do not merge them into a single sweep.\n")
	for i, pass := range plan.Passes {
		brief := passBriefs[pass]
		if brief == "" {
			brief = "Look only for problems of this kind that the diff introduces."
		}
		b.WriteString(fmt.Sprintf("\n%d. %s pass — %s\n", i+1, pass, brief))
	}
	return b.String()
}

// Instruction is the machine-readable half of the ask: how to report what the
// passes found. Each driver appends it the way its harness takes extra
// instruction — claude through --append-system-prompt, codex and opencode
// folded into the prompt — and in review mode NONE of them appends the
// implementer's instruction, so no <verify> or <pr_title> trailer is asked for
// or produced.
//
// It lives here rather than in the drivers because the trailer format is a
// property of the review contract, not of any one harness; three copies would
// be three things to keep in step with the extractor.
func Instruction(passes []string) string {
	if len(passes) == 0 {
		passes = []string{PassSecurity, PassCorrectness}
	}
	return reviewTrailerInstruction(passes)
}

func reviewTrailerInstruction(passes []string) string {
	return fmt.Sprintf(`

[How to report]
Your final message must end with ONE <review> block. Put the opening and closing tags on lines of their own, with compact JSON between them:

<review>
{"findings":[{"key":"sec-auth-missing","parameter":"security","severity":"high","location":"0-acme/api/handler.go:41","what":"the new /export handler does not check the caller's session","why":"any unauthenticated caller can read another org's data"}],"coverage":[{"parameter":"security","state":"checked"},{"parameter":"correctness","state":"checked"}]}
</review>

Rules for the block:
- parameter is one of: %s. severity is one of: info, low, medium, high, critical. state is one of: checked, skipped.
- Report a finding ONLY for a problem the diff introduces or leaves open, with a location a reader can open. Do not report style preferences, pre-existing issues the diff does not touch, or things you would have done differently.
- key is a short stable slug for the finding (parameter + location + rule), so the same finding is recognisable if you see this change again after a fix.
- what is what you saw; why is why it matters. Keep both to a few sentences.
- If a pass found nothing, say so with coverage state "checked" and no findings for it. An empty findings list is a legitimate and common answer.
- Emit the block once, at the very end. Everything outside it is prose for a human and will be shown in the pull request.`,
		strings.Join(passes, ", "))
}

package review

import (
	"strings"
	"testing"

	"github.com/deployment-io/agentbox/internal/config"
)

// These pin the two halves of the fix for an observed must-fix loop failure: a
// Task whose spec required an unauthenticated route returning all of
// process.env was rated Critical in round 1, re-reported under fresh keys in
// round 2, and finally rated Info "because the spec requires it" — after fix
// rounds that added a comment, some logging and a README section. Nothing the
// reviewer was shown made it easy to say "that same problem is still there".

var openFinding = config.ReviewOpenFinding{
	Key:       "sec-unauthenticated-env-dump-app-js",
	Parameter: "security",
	Severity:  "critical",
	Location:  "0-acme/api/app.js",
	What:      "GET /env returns all of process.env with no authentication",
}

func reviewConfig(t *testing.T, open []config.ReviewOpenFinding) *config.Config {
	t.Helper()
	return &config.Config{
		WorkDir:            t.TempDir(),
		ReviewSpec:         `{"title":"Expose the environment"}`,
		ReviewPasses:       []string{PassSecurity, PassCorrectness},
		ReviewRound:        2,
		ReviewOpenFindings: open,
	}
}

// severityPlaces is everywhere the scale has to be stated: each pass brief,
// because a pass rates while it looks, and the trailer rules, because the
// report format is where the word is finally written down. A pass that reads a
// different scale from the one in the rules is a pass whose findings arrive
// under the wrong threshold.
func severityPlaces() []struct {
	name string
	text string
} {
	return []struct {
		name string
		text string
	}{
		{"the security brief", passBriefs[PassSecurity]},
		{"the correctness brief", passBriefs[PassCorrectness]},
		{"the trailer rules", reviewTrailerInstruction([]string{PassSecurity, PassCorrectness}, nil)},
	}
}

// A severity says how much harm the code can do. The spec can make a finding
// EXPECTED, and the consumer's policy can decide to ship it anyway, but a
// requirement cannot make dangerous code harmless — so the rule is put in
// front of the reviewer while it looks and again while it writes the block.
func TestSeverityRuleIsInEveryPassBriefAndTheTrailerRules(t *testing.T) {
	sentences := []string{
		"Rate severity by the harm the code can cause as written.",
		"A requirement in the spec never lowers a finding's severity",
		"Comments, documentation, logging or a README note do not reduce a finding's severity unless they change what the code does.",
	}
	for _, where := range severityPlaces() {
		for _, s := range sentences {
			if !strings.Contains(where.text, s) {
				t.Errorf("%s does not carry %q", where.name, s)
			}
		}
	}
}

// The five words need definitions, or the reviewer falls back on a private
// "likelihood times impact" sense and rates a silent leak Low. These are the
// findings that were rated Low in live reviews: a reaper that left a Job
// running forever, a review container that left a repository writable while
// promising read-only, a flag that dropped a sandbox without checking the
// claim it relied on, an interrupted round that could mark a must-fix
// "resolved". All four are the rubric's medium, and the two rules are why —
// an uncommon trigger and a partial mitigation are not discounts.
func TestSeverityRubricIsInEveryPassBriefAndTheTrailerRules(t *testing.T) {
	definitions := []string{
		"critical — exploitable as written, secrets or credentials exposed, or data lost or corrupted.",
		"high — a core behaviour or a security guarantee breaks in normal use.",
		"medium — the change's own stated guarantee can be bypassed or silently fail under a plausible condition; or a resource leaks (a process, container, connection, lock or file that is never released); or a result is silently wrong.",
		"low — a real defect whose failure is visible and harmless: the caller gets a clear error, or the output is cosmetically wrong.",
		"info — an observation, not a defect.",
		"Do not lower a severity because the triggering condition is uncommon when the failure is silent or defeats what the change is for. This review runs on every change, so an uncommon path is exercised regularly, and a silent failure is found only after it has done damage.",
		"Other controls lower a severity only when they fully prevent the harm, not when they merely limit it. Say which controls you relied on in 'why'.",
	}
	for _, where := range severityPlaces() {
		for _, s := range definitions {
			if !strings.Contains(where.text, s) {
				t.Errorf("%s does not carry %q", where.name, s)
			}
		}
	}
}

// One statement of the scale per place that needs it: once in each pass brief
// and once in the trailer rules. A rubric repeated inside a prompt is prompt
// the reviewer skims, and a second copy is a second thing to keep in step.
func TestSeverityRubricAppearsOncePerPassBriefAndOnceInTheTrailer(t *testing.T) {
	cfg := reviewConfig(t, nil)
	passes := []string{PassSecurity, PassCorrectness}
	prompt := buildPrompt(cfg, Plan{Passes: passes})

	if got := strings.Count(prompt, severityRubric); got != len(passes) {
		t.Errorf("the prompt carries the rubric %d time(s), want once per pass brief (%d)", got, len(passes))
	}
	if got := strings.Count(reviewTrailerInstruction(passes, nil), severityRubric); got != 1 {
		t.Errorf("the trailer rules carry the rubric %d time(s), want 1", got)
	}
}

// A key with a line number in it changes every time the file shifts, so the
// same problem arrives next round looking like a new one — which is how the
// same exposure got re-reported under fresh keys instead of being recognised
// as the Critical that was never fixed.
func TestTrailerRulesForbidALineNumberInTheKey(t *testing.T) {
	instruction := reviewTrailerInstruction([]string{PassSecurity, PassCorrectness}, nil)
	for _, s := range []string{
		"key is a short stable slug naming the parameter, the file and the rule",
		"sec-unauthenticated-env-dump-app-js",
		"Never include a line number",
		"the key must stay the same for the same problem",
	} {
		if !strings.Contains(instruction, s) {
			t.Errorf("the trailer rules do not carry %q", s)
		}
	}
	if strings.Contains(instruction, "(parameter + location + rule)") {
		t.Error("the trailer rules still describe the key as parameter + location + rule, which invites a line number")
	}
}

// The status list is asked for ONLY when findings were handed over. On a first
// round there is nothing to report a status for, and asking anyway invites an
// agent to invent entries for findings nobody made.
func TestTrailerRulesDocumentPreviousOnlyWhenFindingsAreGiven(t *testing.T) {
	without := reviewTrailerInstruction([]string{PassSecurity}, nil)
	if strings.Contains(without, `"previous"`) {
		t.Errorf("the trailer rules ask for a previous-status list with no findings given:\n%s", without)
	}

	with := reviewTrailerInstruction([]string{PassSecurity}, []config.ReviewOpenFinding{openFinding})
	for _, s := range []string{
		`"previous"`,
		`"status":"resolved"|"still_present"`,
		`"note":"<one sentence>"`,
		"ONE ENTRY PER LISTED FINDING",
		"using the key exactly as it was given to you",
	} {
		if !strings.Contains(with, s) {
			t.Errorf("the trailer rules do not document %q:\n%s", s, with)
		}
	}
}

// The prompt opens by telling the reviewer it is not implementing anything and
// must not touch the tree. That stays whether or not the runner mounted the
// repositories read-only — it aims the run at reading — but it must never be
// softened into a claim that the tree is off limits only by request, which
// would read as an invitation to try. So: the instruction is present, and the
// prompt makes no promise about it either way.
func TestPromptForbidsTouchingTheWorkingTree(t *testing.T) {
	cfg := reviewConfig(t, nil)
	for _, readOnlyMounts := range []bool{false, true} {
		cfg.ReviewReadOnlyMounts = readOnlyMounts
		prompt := buildPrompt(cfg, Plan{Passes: []string{PassSecurity}})

		for _, s := range []string{
			"You are NOT implementing anything",
			"do not edit, create or delete any file",
			"do not run any command that changes the working tree",
		} {
			if !strings.Contains(prompt, s) {
				t.Errorf("with read-only mounts=%t the prompt dropped %q:\n%s", readOnlyMounts, s, prompt)
			}
		}
		// Nothing in the prompt may characterise the rule as merely asked
		// for, or as enforced by a mechanism the reviewer could test.
		for _, claim := range []string{"read-only mount", "mounted read-only", "but a prompt is a request", "if you try"} {
			if strings.Contains(strings.ToLower(prompt), strings.ToLower(claim)) {
				t.Errorf("the prompt claims something about enforcement (%q):\n%s", claim, prompt)
			}
		}
	}
}

// The previously-reported section sits after the change index and before the
// passes: the reviewer has just been told where the code is and has not yet
// started looking, so the status question is answered by reading the code.
func TestPromptCarriesThePreviouslyReportedSection(t *testing.T) {
	cfg := reviewConfig(t, []config.ReviewOpenFinding{openFinding})
	prompt := buildPrompt(cfg, Plan{Passes: []string{PassSecurity, PassCorrectness}})

	section := strings.Index(prompt, "[Previously reported issues — check each one]")
	if section < 0 {
		t.Fatalf("the prompt has no previously-reported section:\n%s", prompt)
	}
	index := strings.Index(prompt, "[The change under review]")
	passes := strings.Index(prompt, "[Passes]")
	if !(index < section && section < passes) {
		t.Errorf("section at %d is not between the change index (%d) and the passes (%d)", section, index, passes)
	}

	for _, s := range []string{
		openFinding.Key,
		openFinding.Parameter,
		openFinding.Severity,
		openFinding.Location,
		openFinding.What,
		"read the CURRENT code and decide whether the problem is still present",
		"'resolved' means the code no longer has the problem",
		"A comment, documentation, logging, a README note, or a spec requirement does NOT resolve it.",
		"a problem that is still present is reported in the status list, not repeated as a new finding",
	} {
		if !strings.Contains(prompt, s) {
			t.Errorf("the prompt does not carry %q", s)
		}
	}
}

// And nothing of the sort on a round with nothing open.
func TestPromptOmitsThePreviouslyReportedSectionWhenNothingIsOpen(t *testing.T) {
	cfg := reviewConfig(t, nil)
	prompt := buildPrompt(cfg, Plan{Passes: []string{PassSecurity}})

	if strings.Contains(prompt, "[Previously reported issues") {
		t.Errorf("the prompt carries a previously-reported section with nothing open:\n%s", prompt)
	}
	if !strings.Contains(prompt, "you have not been shown the earlier round's findings") &&
		!strings.Contains(prompt, "You have not been shown the earlier round's findings") {
		t.Error("a later round with nothing open should still say the earlier findings were not shown")
	}
}

// The review's blindness to the IMPLEMENTER is what the open-findings input
// must not erode: the reviewer sees its own previous findings and nothing the
// implementer wrote about them.
func TestPromptCarriesNothingTheImplementerWrote(t *testing.T) {
	cfg := reviewConfig(t, []config.ReviewOpenFinding{openFinding})
	cfg.PreviousStepsSummary = "I fixed the env route by adding a comment explaining it."
	cfg.StepPrompt = "add a README section about the env route"
	prompt := buildPrompt(cfg, Plan{Passes: []string{PassSecurity}})

	for _, leaked := range []string{"I fixed the env route", "add a README section"} {
		if strings.Contains(prompt, leaked) {
			t.Errorf("the review prompt carries the implementer's words: %q", leaked)
		}
	}
}
